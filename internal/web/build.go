package web

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"runtime"
	"time"
)

const (
	// buildIDLength is how many characters of the program's SHA-256 name a build.
	buildIDLength = 7
	// developmentVersion is what a build not made from a release tag calls itself.
	developmentVersion = "dev"
)

// releaseLabel names a release as the header shows it: v0.1.0, or dev for any other build.
func releaseLabel(version string) string {
	if version == "" || version == developmentVersion {
		return developmentVersion
	}
	return "v" + version
}

// build names the running program by its own contents and the time it was made, so
// every new build shows a new name without anyone typing a version in.
type build struct {
	ID    string
	Built time.Time
	Go    string
}

func readBuild() build {
	running := build{Go: runtime.Version()}
	path, err := os.Executable()
	if err != nil {
		return running
	}
	file, err := os.Open(path)
	if err != nil {
		return running
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return running
	}
	running.ID = hex.EncodeToString(hash.Sum(nil))[:buildIDLength]
	if info, err := file.Stat(); err == nil {
		running.Built = info.ModTime()
	}
	return running
}
