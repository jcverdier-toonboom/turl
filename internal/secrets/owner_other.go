//go:build !unix

package secrets

import "os"

func owner(os.FileInfo) (int, bool) { return 0, false }
