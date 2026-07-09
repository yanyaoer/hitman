package hitman

// Fold state machine adapted from cpa-plugin-codexcomp/executor.go (MIT). Two I/O
// boundaries differ from the original: the upstream host-callback stream is
// replaced by openRoundFn (a real streaming POST via socks), and the downstream
// host.stream.emit is replaced by emit (writing SSE straight to the codex client).
// The algorithm itself is kept identical. See NOTICE.

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
)

const maxSSEBufferSize = 8 * 1024 * 1024

type foldState struct {
	baseBody  map[string]any
	origInput []any

	emit        func(payload []byte) error
	openRoundFn func(body []byte) (int, io.ReadCloser, error)

	roundNo      int
	dsOI         int
	seq          int
	baseResponse map[string]any
	finalOutput  []map[string]any
	replayTail   []any
	summedUsage  map[string]any
	firstUsage   map[string]any
	roundsInfo   []map[string]any

	roundReasoning []map[string]any
	kind           map[int]string
	oiToDS         map[int]int
	buffered       []bufferedEntry
	terminal       map[string]any
	usage          map[string]any

	sseBuffer []byte
	config    foldConfig
}

type bufferedEntry struct {
	oi     int
	item   map[string]any
	events []map[string]any
}

type upstreamError struct {
	status int
	msg    string
}

func (e *upstreamError) Error() string { return e.msg }

type midStreamError struct{ msg string }

func (e *midStreamError) Error() string { return e.msg }

func newFoldState(baseBody map[string]any, origInput []any, emit func([]byte) error, openRoundFn func([]byte) (int, io.ReadCloser, error), cfg foldConfig) *foldState {
	return &foldState{
		baseBody:    baseBody,
		origInput:   origInput,
		emit:        emit,
		openRoundFn: openRoundFn,
		summedUsage: map[string]any{},
		kind:        map[int]string{},
		oiToDS:      map[int]int{},
		config:      cfg,
	}
}

func sseEvent(ev map[string]any) []byte {
	raw, _ := json.Marshal(ev)
	return append([]byte("data: "), append(raw, '\n', '\n')...)
}

func (fs *foldState) emitDone() error { return fs.emit([]byte("data: [DONE]\n\n")) }

func (fs *foldState) run() {
	for {
		terminal, usage, roundErr := fs.openRound()
		if roundErr != nil {
			var fev map[string]any
			if _, isMid := roundErr.(*midStreamError); isMid {
				fev = fs.incompleteEvent("upstream_error")
			} else if fs.roundNo == 1 {
				status := 502
				if ue, ok := roundErr.(*upstreamError); ok {
					status = ue.status
				}
				fev = failedEvent(status, roundErr.Error())
			} else {
				fev = fs.incompleteEvent("upstream_error")
			}
			fs.stamp(fev)
			_ = fs.emit(sseEvent(fev))
			_ = fs.emitDone()
			return
		}
		if terminal == nil {
			iev := fs.incompleteEvent("upstream_eof")
			fs.stamp(iev)
			_ = fs.emit(sseEvent(iev))
			_ = fs.emitDone()
			return
		}
		fs.endRound(terminal, usage)
		if fs.shouldContinue() {
			fs.debugf("continuing after round=%d reasoning_tokens=%s", fs.roundNo, optionalIntString(reasoningTokens(fs.usage)))
			fs.prepareNextRound()
			continue
		}
		if err := fs.flushCleanStop(); err != nil {
			_ = fs.emitDone()
			return
		}
		ev := fs.terminalEvent()
		fs.stamp(ev)
		if err := fs.emit(sseEvent(ev)); err != nil {
			return
		}
		_ = fs.emitDone()
		return
	}
}

func (fs *foldState) openRound() (map[string]any, map[string]any, error) {
	fs.roundNo++
	fs.roundReasoning = nil
	fs.kind = map[int]string{}
	fs.oiToDS = map[int]int{}
	fs.buffered = nil
	fs.terminal = nil
	fs.usage = nil
	fs.sseBuffer = nil

	input := append(append([]any{}, fs.origInput...), fs.replayTail...)
	bodyBytes, err := json.Marshal(nextRoundBody(fs.baseBody, input))
	if err != nil {
		return nil, nil, &upstreamError{status: 500, msg: err.Error()}
	}

	status, body, err := fs.openRoundFn(bodyBytes)
	if err != nil {
		return nil, nil, &upstreamError{status: 502, msg: err.Error()}
	}
	defer body.Close()
	if status >= 400 {
		detail, _ := io.ReadAll(io.LimitReader(body, 8192))
		return nil, nil, &upstreamError{status: status, msg: fmt.Sprintf("upstream status %d: %s", status, string(detail))}
	}

	buf := make([]byte, 32*1024)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			term, perr := fs.processAndEmit(buf[:n])
			if perr != nil {
				return nil, nil, &midStreamError{msg: perr.Error()}
			}
			if term != nil {
				return fs.terminal, fs.usage, nil
			}
		}
		if readErr == io.EOF {
			return fs.terminal, fs.usage, nil
		}
		if readErr != nil {
			return nil, nil, &midStreamError{msg: readErr.Error()}
		}
	}
}

func (fs *foldState) processAndEmit(payload []byte) (map[string]any, error) {
	fs.sseBuffer = append(fs.sseBuffer, payload...)
	if len(fs.sseBuffer) > maxSSEBufferSize {
		return nil, fmt.Errorf("sse buffer exceeded %d bytes", maxSSEBufferSize)
	}
	for {
		dataStart := findSubstring(fs.sseBuffer, []byte("data:"))
		if dataStart < 0 {
			break
		}
		jsonStart := dataStart + 5
		for jsonStart < len(fs.sseBuffer) && (fs.sseBuffer[jsonStart] == ' ' || fs.sseBuffer[jsonStart] == '\t') {
			jsonStart++
		}
		if jsonStart >= len(fs.sseBuffer) {
			break
		}
		// "[DONE]" is 6 bytes; the cpa original compares only 5 (a latent off-by-one,
		// harmless there because the terminal event returns first). Fixed here.
		if jsonStart+6 <= len(fs.sseBuffer) && string(fs.sseBuffer[jsonStart:jsonStart+6]) == "[DONE]" {
			fs.sseBuffer = fs.sseBuffer[jsonStart+6:]
			continue
		}
		if fs.sseBuffer[jsonStart] != '{' {
			fs.sseBuffer = fs.sseBuffer[dataStart+5:]
			continue
		}
		jsonEnd := findJSONEnd(fs.sseBuffer, jsonStart)
		if jsonEnd < 0 {
			break
		}
		dataBytes := fs.sseBuffer[jsonStart : jsonEnd+1]
		fs.sseBuffer = fs.sseBuffer[jsonEnd+1:]
		var ev map[string]any
		if err := json.Unmarshal(dataBytes, &ev); err != nil {
			return nil, fmt.Errorf("parse SSE data: %w", err)
		}
		term, err := fs.processEvent(ev)
		if err != nil {
			return nil, err
		}
		if term != nil {
			return term, nil
		}
	}
	return nil, nil
}

func findSubstring(data, sub []byte) int {
	if len(sub) == 0 || len(data) < len(sub) {
		return -1
	}
	for i := 0; i <= len(data)-len(sub); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			if data[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func findJSONEnd(data []byte, start int) int {
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(data); i++ {
		c := data[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' {
			escaped = true
			continue
		}
		if c == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		if c == '{' {
			depth++
		} else if c == '}' {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func (fs *foldState) processEvent(ev map[string]any) (map[string]any, error) {
	etype, _ := ev["type"].(string)

	if etype == "response.created" || etype == "response.in_progress" {
		if fs.roundNo == 1 {
			if etype == "response.created" {
				if r, ok := ev["response"].(map[string]any); ok {
					fs.baseResponse = r
				}
			}
			fs.stamp(ev)
			if err := fs.emit(sseEvent(ev)); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}

	if terminalTypes[etype] {
		fs.terminal = ev
		if r, ok := ev["response"].(map[string]any); ok {
			if u, ok := r["usage"].(map[string]any); ok {
				fs.usage = u
			}
		}
		return ev, nil
	}

	oi := -1
	if v, ok := ev["output_index"].(float64); ok {
		oi = int(v)
	}

	if etype == "response.output_item.added" {
		item, _ := ev["item"].(map[string]any)
		if item == nil {
			item = map[string]any{}
		}
		itemType, _ := item["type"].(string)
		if itemType == "reasoning" {
			fs.kind[oi] = "reasoning"
			fs.oiToDS[oi] = fs.dsOI
			ev["output_index"] = fs.dsOI
			fs.dsOI++
			fs.stamp(ev)
			if err := fs.emit(sseEvent(ev)); err != nil {
				return nil, err
			}
		} else {
			fs.kind[oi] = "buffered"
			fs.buffered = append(fs.buffered, bufferedEntry{oi: oi, item: item, events: []map[string]any{ev}})
		}
		return nil, nil
	}

	k := fs.kind[oi]
	if k == "reasoning" {
		if ds, ok := fs.oiToDS[oi]; ok {
			ev["output_index"] = ds
		}
		if etype == "response.output_item.done" {
			if item, ok := ev["item"].(map[string]any); ok {
				fs.roundReasoning = append(fs.roundReasoning, item)
				fs.finalOutput = append(fs.finalOutput, item)
			}
		}
		fs.stamp(ev)
		if err := fs.emit(sseEvent(ev)); err != nil {
			return nil, err
		}
	} else if k == "buffered" {
		for i := range fs.buffered {
			if fs.buffered[i].oi == oi {
				fs.buffered[i].events = append(fs.buffered[i].events, ev)
				if etype == "response.output_item.done" {
					if item, ok := ev["item"].(map[string]any); ok {
						fs.buffered[i].item = item
					}
				}
				break
			}
		}
	} else {
		fs.stamp(ev)
		if err := fs.emit(sseEvent(ev)); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func (fs *foldState) stamp(ev map[string]any) {
	ev["sequence_number"] = fs.seq
	fs.seq++
}

func (fs *foldState) endRound(terminal map[string]any, usage map[string]any) {
	fs.terminal = terminal
	fs.usage = usage
	sumUsage(fs.summedUsage, usage)
	if fs.roundNo == 1 {
		fs.firstUsage = usage
	}
	rt := reasoningTokens(usage)
	n := tierNWithStep(rt, fs.config.TruncationStep)
	fs.roundsInfo = append(fs.roundsInfo, map[string]any{
		"round":            fs.roundNo,
		"reasoning_tokens": rt,
		"n":                n,
	})
	fs.debugf("round=%d completed reasoning_tokens=%s tier=%s", fs.roundNo, optionalIntString(rt), optionalIntString(n))
}

func (fs *foldState) shouldContinue() bool {
	if fs.terminal == nil {
		return false
	}
	etype, _ := fs.terminal["type"].(string)
	if etype != "response.completed" {
		return false
	}
	rt := reasoningTokens(fs.usage)
	n := tierNWithStep(rt, fs.config.TruncationStep)
	if !inContinueWindowWithMax(n, fs.config.MaxTierN) {
		return false
	}
	if !fs.hasEncryptedContent() {
		return false
	}
	return fs.roundNo <= fs.config.MaxContinue
}

func (fs *foldState) stoppedReason() string {
	if fs.terminal == nil {
		return ""
	}
	etype, _ := fs.terminal["type"].(string)
	if etype != "response.completed" {
		return ""
	}
	rt := reasoningTokens(fs.usage)
	n := tierNWithStep(rt, fs.config.TruncationStep)
	if n == nil {
		return ""
	}
	if !fs.hasEncryptedContent() {
		return "no_encrypted_content"
	}
	if fs.roundNo > fs.config.MaxContinue {
		return "max_continue"
	}
	return "tier_out_of_window"
}

func (fs *foldState) hasEncryptedContent() bool {
	if len(fs.roundReasoning) == 0 {
		return false
	}
	last := fs.roundReasoning[len(fs.roundReasoning)-1]
	s, ok := last["encrypted_content"].(string)
	return ok && s != ""
}

func (fs *foldState) prepareNextRound() {
	tail := make([]any, 0, len(fs.roundReasoning)+1)
	for _, r := range fs.roundReasoning {
		tail = append(tail, r)
	}
	tail = append(tail, commentaryNudge(fs.config.MarkerText))
	fs.replayTail = append(fs.replayTail, tail...)
}

func (fs *foldState) debugf(format string, args ...any) {
	if !fs.config.DebugLog {
		return
	}
	log.Printf("[fold] "+format, args...)
}

func optionalIntString(value *int) string {
	if value == nil {
		return ""
	}
	return fmt.Sprintf("%d", *value)
}

func (fs *foldState) flushCleanStop() error {
	for _, entry := range fs.buffered {
		for _, ev := range entry.events {
			if _, ok := ev["output_index"]; ok {
				ev["output_index"] = fs.dsOI
			}
			fs.stamp(ev)
			if err := fs.emit(sseEvent(ev)); err != nil {
				return err
			}
		}
		fs.dsOI++
		fs.finalOutput = append(fs.finalOutput, entry.item)
	}
	return nil
}

func (fs *foldState) terminalEvent() map[string]any {
	return terminalEvent(
		fs.terminal,
		fs.baseResponse,
		fs.finalOutput,
		agentUsage(fs.firstUsage, fs.summedUsage, fs.usage, true),
		fs.roundsInfo,
		fs.summedUsage,
		fs.stoppedReason(),
		"",
	)
}

func (fs *foldState) incompleteEvent(reason string) map[string]any {
	return terminalEvent(
		nil,
		fs.baseResponse,
		fs.finalOutput,
		agentUsage(fs.firstUsage, fs.summedUsage, fs.usage, false),
		fs.roundsInfo,
		fs.summedUsage,
		reason,
		reason,
	)
}
