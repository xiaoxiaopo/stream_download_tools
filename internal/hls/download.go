package hls

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func Download(ctx context.Context, opt Options) error {
	if opt.Output == "" {
		return errors.New("output path is required")
	}
	if _, err := os.Stat(opt.Output); err == nil {
		return fmt.Errorf("output file already exists; refusing to overwrite: %s", opt.Output)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check output file: %w", err)
	}
	if !strings.EqualFold(filepath.Ext(opt.Output), ".ts") && opt.FFmpegPath == "" {
		return fmt.Errorf("output extension %q requires --ffmpeg for remuxing", filepath.Ext(opt.Output))
	}
	if opt.Workers < 1 {
		opt.Workers = 1
	}
	if opt.Retries < 0 {
		opt.Retries = 0
	}
	parsed, err := url.Parse(opt.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("url must be an http or https URL")
	}
	if err := os.MkdirAll(filepath.Dir(opt.Output), 0755); err != nil {
		return err
	}
	if opt.WorkDir == "" {
		opt.WorkDir = opt.Output + ".work"
	}
	if err := os.MkdirAll(opt.WorkDir, 0755); err != nil {
		return err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if strings.TrimSpace(opt.Proxy) != "" {
		proxy, parseErr := url.Parse(strings.TrimSpace(opt.Proxy))
		if parseErr != nil || proxy.Host == "" || (proxy.Scheme != "http" && proxy.Scheme != "https" && proxy.Scheme != "socks5" && proxy.Scheme != "socks5h") {
			return errors.New("代理地址无效：请使用 http://、https:// 或 socks5:// 地址")
		}
		transport.Proxy = http.ProxyURL(proxy)
	}
	client := &http.Client{Timeout: 45 * time.Second, Transport: transport}
	f := &fetcher{client: client, headers: opt.Headers.Clone(), retries: opt.Retries, workDir: opt.WorkDir, keys: make(map[string][]byte)}
	if f.headers == nil {
		f.headers = make(http.Header)
	}

	manifest, err := f.get(ctx, opt.URL, "")
	if err != nil {
		return fmt.Errorf("fetch playlist: %w", err)
	}
	manifestURL := opt.URL
	if variantURL, isMaster, err := parseMaster(manifest, parsed, opt.MaxHeight); err != nil {
		return err
	} else if isMaster {
		manifestURL = variantURL
		manifest, err = f.get(ctx, manifestURL, "")
		if err != nil {
			return fmt.Errorf("fetch selected variant playlist: %w", err)
		}
	}
	base, _ := url.Parse(manifestURL)
	p, err := parseMedia(manifest, base)
	if err != nil {
		return err
	}
	if !p.EndList {
		return errors.New("live HLS playlists are not yet supported; a finite playlist with EXT-X-ENDLIST is required")
	}
	if p.InitURI != "" && strings.EqualFold(filepath.Ext(opt.Output), ".ts") {
		return errors.New("fragmented MP4 HLS input cannot be written as .ts; choose an MP4 output and configure --ffmpeg")
	}

	statePath := filepath.Join(opt.WorkDir, "resume.json")
	state, err := loadState(statePath, manifestURL, p.InitURI, p.Segments)
	if err != nil {
		return err
	}
	if p.InitURI != "" {
		initPath := filepath.Join(opt.WorkDir, "init.bin")
		initData, err := f.get(ctx, p.InitURI, "")
		if err != nil {
			return fmt.Errorf("fetch HLS initialization section: %w", err)
		}
		if err := os.WriteFile(initPath+".tmp", initData, 0600); err != nil {
			return err
		}
		if err := replaceFile(initPath+".tmp", initPath); err != nil {
			return err
		}
	}
	var mu sync.Mutex
	completedCount := 0
	for _, done := range state.Completed {
		if done {
			completedCount++
		}
	}
	if opt.Progress != nil {
		opt.Progress(Progress{Done: completedCount, Total: len(p.Segments)})
	}
	jobs := make(chan job)
	results := make(chan result, opt.Workers)
	var workers sync.WaitGroup
	for w := 0; w < opt.Workers; w++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for j := range jobs {
				mu.Lock()
				alreadyDone := state.Completed[j.index]
				mu.Unlock()
				if alreadyDone {
					results <- result{index: j.index}
					continue
				}
				path := filepath.Join(opt.WorkDir, fmt.Sprintf("segment-%08d.bin", j.index))
				err := f.downloadSegment(ctx, j.seg, path)
				if err == nil {
					mu.Lock()
					state.Completed[j.index] = true
					err = saveState(statePath, state)
					if err == nil {
						completedCount++
					}
					count := completedCount
					mu.Unlock()
					if err == nil && opt.Progress != nil {
						opt.Progress(Progress{Done: count, Total: len(p.Segments)})
					}
				}
				results <- result{index: j.index, err: err}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for i, seg := range p.Segments {
			mu.Lock()
			done := state.Completed[i]
			mu.Unlock()
			if done {
				continue
			}
			select {
			case jobs <- job{index: i, seg: seg}:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { workers.Wait(); close(results) }()
	var firstErr error
	for res := range results {
		if res.err != nil && firstErr == nil {
			firstErr = res.err
		}
	}
	if firstErr != nil {
		return firstErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	merged := filepath.Join(opt.WorkDir, "merged.ts")
	if err := concatenate(opt.WorkDir, merged, len(p.Segments), p.InitURI != ""); err != nil {
		return err
	}
	if strings.EqualFold(filepath.Ext(opt.Output), ".ts") {
		return copyFile(merged, opt.Output)
	}
	if opt.FFmpegPath == "" {
		return fmt.Errorf("output extension %q requires --ffmpeg for remuxing", filepath.Ext(opt.Output))
	}
	ext := filepath.Ext(opt.Output)
	remuxOutput := filepath.Join(opt.WorkDir, "remux"+ext)
	_ = os.Remove(remuxOutput)
	defer os.Remove(remuxOutput)
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-i", merged, "-c", "copy"}
	if strings.EqualFold(filepath.Ext(opt.Output), ".mp4") || strings.EqualFold(filepath.Ext(opt.Output), ".mov") {
		args = append(args, "-movflags", "+faststart")
	}
	args = append(args, remuxOutput)
	cmd := exec.CommandContext(ctx, opt.FFmpegPath, args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg remux failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return copyFileNoReplace(remuxOutput, opt.Output)
}

func loadState(path, source, initURI string, segments []segment) (resumeState, error) {
	state := resumeState{Version: 1, Source: source, InitURI: initURI, Segments: make([]string, len(segments)), Completed: make([]bool, len(segments))}
	for i, s := range segments {
		state.Segments[i] = segmentIdentity(s)
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, saveState(path, state)
	}
	if err != nil {
		return state, err
	}
	var old resumeState
	if json.Unmarshal(data, &old) != nil || old.Version != state.Version || old.Source != source || old.InitURI != initURI || !equalStrings(old.Segments, state.Segments) || len(old.Completed) != len(segments) {
		return state, saveState(path, state)
	}
	state.Completed = old.Completed
	for i, done := range state.Completed {
		if done {
			if _, err := os.Stat(filepath.Join(filepath.Dir(path), fmt.Sprintf("segment-%08d.bin", i))); err != nil {
				state.Completed[i] = false
			}
		}
	}
	return state, saveState(path, state)
}

func saveState(path string, state resumeState) error {
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return replaceFile(tmp, path)
}

func replaceFile(src, dst string) error {
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(src, dst)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (f *fetcher) get(ctx context.Context, rawURL, byteRange string) ([]byte, error) {
	var last error
	for attempt := 0; attempt <= f.retries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header = f.headers.Clone()
		if req.Header.Get("User-Agent") == "" {
			req.Header.Set("User-Agent", "HLSGo/0.1")
		}
		if byteRange != "" {
			req.Header.Set("Range", byteRange)
		}
		resp, err := f.client.Do(req)
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr == nil && (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent) {
				return body, nil
			}
			if readErr != nil {
				err = readErr
			} else {
				err = fmt.Errorf("HTTP %s for %s", resp.Status, rawURL)
			}
		}
		last = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt < f.retries {
			select {
			case <-time.After(time.Duration(attempt+1) * 400 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return nil, last
}

func (f *fetcher) downloadSegment(ctx context.Context, seg segment, path string) error {
	cipherPath := path + ".cipher.tmp"
	defer os.Remove(cipherPath)
	var err error
	for attempt := 0; attempt <= f.retries; attempt++ {
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, seg.URI, nil)
		if e != nil {
			return e
		}
		req.Header = f.headers.Clone()
		if req.Header.Get("User-Agent") == "" {
			req.Header.Set("User-Agent", "HLSGo/0.1")
		}
		if seg.HasRange {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", seg.RangeStart, seg.RangeStart+seg.RangeLength-1))
		}
		resp, e := f.client.Do(req)
		if e == nil {
			validStatus := (!seg.HasRange && resp.StatusCode == http.StatusOK) || (seg.HasRange && resp.StatusCode == http.StatusPartialContent)
			if !validStatus {
				e = fmt.Errorf("HTTP %s downloading segment", resp.Status)
				resp.Body.Close()
			} else {
				file, createErr := os.Create(cipherPath)
				if createErr != nil {
					resp.Body.Close()
					return createErr
				}
				_, copyErr := io.Copy(file, resp.Body)
				bodyCloseErr := resp.Body.Close()
				fileCloseErr := file.Close()
				switch {
				case copyErr != nil:
					e = copyErr
				case bodyCloseErr != nil:
					e = bodyCloseErr
				case fileCloseErr != nil:
					e = fileCloseErr
				default:
					err = nil
					break
				}
				if err == nil {
					break
				}
			}
		}
		err = e
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt < f.retries {
			select {
			case <-time.After(time.Duration(attempt+1) * 400 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if seg.Key != nil {
		key, err := f.key(ctx, seg.Key.URI)
		if err != nil {
			return err
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return err
		}
		iv := seg.Key.IV
		if len(iv) == 0 {
			iv = make([]byte, aes.BlockSize)
			for i := 0; i < 8; i++ {
				iv[15-i] = byte(seg.Sequence >> (8 * i))
			}
		}
		if err := decryptAES128CBCFile(cipherPath, tmp, block, iv); err != nil {
			return err
		}
	} else {
		if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Rename(cipherPath, tmp); err != nil {
			return err
		}
	}
	return replaceFile(tmp, path)
}

func decryptAES128CBCFile(source, destination string, block cipher.Block, iv []byte) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 || info.Size()%aes.BlockSize != 0 {
		return errors.New("AES-128 segment has invalid ciphertext length")
	}
	output, err := os.Create(destination)
	if err != nil {
		return err
	}
	completed := false
	defer func() {
		output.Close()
		if !completed {
			os.Remove(destination)
		}
	}()
	decrypter := cipher.NewCBCDecrypter(block, iv)
	remaining := info.Size()
	const chunkLimit = 64 * 1024
	for remaining > aes.BlockSize {
		chunkSize := int64(chunkLimit)
		if chunkSize > remaining-aes.BlockSize {
			chunkSize = remaining - aes.BlockSize
		}
		chunkSize -= chunkSize % aes.BlockSize
		ciphertext := make([]byte, int(chunkSize))
		if _, err := io.ReadFull(input, ciphertext); err != nil {
			return err
		}
		plaintext := make([]byte, len(ciphertext))
		decrypter.CryptBlocks(plaintext, ciphertext)
		if _, err := output.Write(plaintext); err != nil {
			return err
		}
		remaining -= chunkSize
	}
	lastCiphertext := make([]byte, aes.BlockSize)
	if _, err := io.ReadFull(input, lastCiphertext); err != nil {
		return err
	}
	lastPlaintext := make([]byte, aes.BlockSize)
	decrypter.CryptBlocks(lastPlaintext, lastCiphertext)
	padding := int(lastPlaintext[len(lastPlaintext)-1])
	if padding < 1 || padding > aes.BlockSize {
		return errors.New("AES-128 segment has invalid PKCS#7 padding")
	}
	for _, value := range lastPlaintext[len(lastPlaintext)-padding:] {
		if int(value) != padding {
			return errors.New("AES-128 segment has invalid PKCS#7 padding")
		}
	}
	if _, err := output.Write(lastPlaintext[:aes.BlockSize-padding]); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	completed = true
	return nil
}

func (f *fetcher) key(ctx context.Context, rawURL string) ([]byte, error) {
	f.keyMu.Lock()
	cached, ok := f.keys[rawURL]
	f.keyMu.Unlock()
	if ok {
		return cached, nil
	}
	data, err := f.get(ctx, rawURL, "")
	if err != nil {
		return nil, fmt.Errorf("fetch AES key: %w", err)
	}
	if len(data) != aes.BlockSize {
		return nil, fmt.Errorf("AES-128 key must be 16 bytes, got %d", len(data))
	}
	f.keyMu.Lock()
	f.keys[rawURL] = data
	f.keyMu.Unlock()
	return data, nil
}

func segmentIdentity(s segment) string {
	keyURI, iv := "", ""
	if s.Key != nil {
		keyURI = s.Key.URI
		iv = fmt.Sprintf("%x", s.Key.IV)
	}
	return fmt.Sprintf("%s|%d|%t|%d|%d|%s|%s", s.URI, s.Sequence, s.HasRange, s.RangeStart, s.RangeLength, keyURI, iv)
}

func concatenate(dir, output string, count int, hasInit bool) error {
	tmp := output + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if hasInit {
		init, err := os.Open(filepath.Join(dir, "init.bin"))
		if err != nil {
			out.Close()
			os.Remove(tmp)
			return err
		}
		_, copyErr := io.Copy(out, init)
		closeErr := init.Close()
		if copyErr != nil {
			out.Close()
			os.Remove(tmp)
			return copyErr
		}
		if closeErr != nil {
			out.Close()
			os.Remove(tmp)
			return closeErr
		}
	}
	for i := 0; i < count; i++ {
		in, err := os.Open(filepath.Join(dir, fmt.Sprintf("segment-%08d.bin", i)))
		if err != nil {
			out.Close()
			os.Remove(tmp)
			return fmt.Errorf("missing segment %d: %w", i, err)
		}
		_, copyErr := io.Copy(out, in)
		closeErr := in.Close()
		if copyErr != nil {
			out.Close()
			os.Remove(tmp)
			return copyErr
		}
		if closeErr != nil {
			out.Close()
			os.Remove(tmp)
			return closeErr
		}
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return replaceFile(tmp, output)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return copyReaderToNewFile(in, dst)
}

func copyFileNoReplace(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return copyReaderToNewFile(in, dst)
}

func copyReaderToNewFile(src io.Reader, dst string) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("output file already exists; refusing to overwrite: %s", dst)
		}
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	return nil
}
