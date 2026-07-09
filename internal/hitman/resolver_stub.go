//go:build !darwin

package hitman

func installResolvers([]string, string) error {
	return nil
}

func cleanupResolvers([]string) error {
	return nil
}
