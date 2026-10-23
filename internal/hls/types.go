package hls

import (
	"net/http"
	"sync"
)

type Options struct {
	URL        string
	Output     string
	WorkDir    string
	Headers    http.Header
	Workers    int
	Retries    int
	MaxHeight  int
	FFmpegPath string
	Proxy      string
	Progress   func(Progress)
}

type Progress struct{ Done, Total int }

type playlist struct {
	Segments []segment
	InitURI  string
	EndList  bool
}

type segment struct {
	URI         string
	Sequence    uint64
	Key         *keyInfo
	RangeStart  int64
	RangeLength int64
	HasRange    bool
}

type keyInfo struct {
	URI string
	IV  []byte
}

type variant struct {
	URI       string
	Bandwidth int64
	Height    int
}

type resumeState struct {
	Version   int      `json:"version"`
	Source    string   `json:"source"`
	InitURI   string   `json:"init_uri,omitempty"`
	Segments  []string `json:"segments"`
	Completed []bool   `json:"completed"`
}

type job struct {
	index int
	seg   segment
}

type result struct {
	index int
	err   error
}

type fetcher struct {
	client  *http.Client
	headers http.Header
	retries int
	workDir string
	keyMu   sync.Mutex
	keys    map[string][]byte
}
