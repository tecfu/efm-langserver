package langserver

import (
    "os"
)



type Stdrwc struct{}

func (Stdrwc) Read(p []byte) (int, error) {
	return os.Stdin.Read(p)
}

func (c Stdrwc) Write(p []byte) (int, error) {
	return os.Stdout.Write(p)
}

func (c Stdrwc) Close() error {
	if err := os.Stdin.Close(); err != nil {
		return err
	}
	return os.Stdout.Close()
}
