//go:build !unix

package pgtest

// lockFile is a no-op where advisory file locks aren't available; concurrent
// first-time extraction into the shared cache is unguarded on those platforms.
func lockFile(path string) (unlock func(), err error) {
	return func() {}, nil
}
