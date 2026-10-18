//go:build !(linux || darwin || freebsd || openbsd || netbsd)

package cli

import "os"

// readKey falls back to a line (key, then Enter) where raw input isn't set up.
func readKey(in *os.File) (byte, error) { return readKeyLine(in) }
