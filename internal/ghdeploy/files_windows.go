package ghdeploy

import "errors"

// GitHub deployment reports work only on Linux: the config refuses a
// [github] block on Windows.
var errWindows = errors.New("GitHub deployment reports work only on Linux")

func ensureDir(string, uint32) error { return errWindows }

// CheckDir is not used on Windows.
func CheckDir(string, uint32) error { return errWindows }

func readFile(string, uint32, int64) ([]byte, error) { return nil, errWindows }
