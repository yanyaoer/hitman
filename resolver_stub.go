//go:build !darwin

package main

func installResolvers([]string, string) error {
	return nil
}

func cleanupResolvers([]string) error {
	return nil
}
