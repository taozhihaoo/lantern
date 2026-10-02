package server

import "os"

func osWrite(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
