package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

const (
	progressRefreshInterval = 100 * time.Millisecond
	progressBarWidth        = 24
	bytesPerMiB             = 1024 * 1024
)

// downloadProgress aggregates all archive downloads belonging to one install
// operation. It intentionally owns both the state and the output lock: a
// download can be updated by any worker, while the renderer must always write
// a complete line to the terminal.
type downloadProgress struct {
	mu sync.Mutex

	output  io.Writer
	enabled bool
	files   map[string]*progressFile

	startedAt      time.Time
	sessionStarted bool
	lineRendered   bool
	closed         bool
	stop           chan struct{}
	stopped        chan struct{}
}

type progressFile struct {
	total      int64
	downloaded int64
	started    bool
	finished   bool
	success    bool
}

func newDownloadProgress(output io.Writer) *downloadProgress {
	if output == nil {
		output = io.Discard
	}
	return &downloadProgress{
		output:  output,
		enabled: terminalWriter(output),
		files:   make(map[string]*progressFile),
	}
}

func terminalWriter(output io.Writer) bool {
	if ci := os.Getenv("CI"); ci != "" && ci != "0" && !strings.EqualFold(ci, "false") {
		return false
	}
	descriptor, ok := output.(interface{ Fd() uintptr })
	if !ok {
		return false
	}
	fd := descriptor.Fd()
	return term.IsTerminal(int(fd))
}

// register records an expected archive before workers start. This lets the
// aggregate include all ordinary install targets even when they begin at
// different times. Bootstrap archives are registered when discovered.
func (p *downloadProgress) register(name string, total int64) {
	if p == nil || name == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	file := p.fileLocked(name)
	if file.total <= 0 && total > 0 {
		file.total = total
	}
}

func (p *downloadProgress) begin(name string, total int64) {
	if p == nil || name == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	file := p.fileLocked(name)
	if file.total <= 0 && total > 0 {
		file.total = total
	}
	file.downloaded = 0
	file.started = true
	file.finished = false
	file.success = false
	if !p.sessionStarted {
		p.sessionStarted = true
		p.startedAt = time.Now()
		p.startTickerLocked()
	}
	p.renderLocked()
}

func (p *downloadProgress) addBytes(name string, count int64) {
	if p == nil || name == "" || count <= 0 {
		return
	}
	p.mu.Lock()
	p.fileLocked(name).downloaded += count
	p.mu.Unlock()
}

func (p *downloadProgress) finish(name string, success bool) {
	if p == nil || name == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	file := p.fileLocked(name)
	file.finished = true
	file.success = success
	if p.sessionStarted {
		p.renderLocked()
		p.clearIfIdleLocked()
	}
}

// cached marks a file as already available locally. It does not start the
// renderer, so an install consisting only of cache hits remains silent.
func (p *downloadProgress) cached(name string, _ int64) {
	if p == nil || name == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// A second consumer may find an archive downloaded by this session.
	// Preserve its real transfer, but exclude pre-existing cache entries.
	if file := p.files[name]; file != nil && !file.started {
		delete(p.files, name)
	}
	if p.sessionStarted {
		p.renderLocked()
		p.clearIfIdleLocked()
	}
}

func (p *downloadProgress) failed(name string) {
	if p == nil || name == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	file := p.fileLocked(name)
	file.finished = true
	file.success = false
	if p.sessionStarted {
		p.renderLocked()
		p.clearIfIdleLocked()
	}
}

func (p *downloadProgress) fileLocked(name string) *progressFile {
	file := p.files[name]
	if file == nil {
		file = &progressFile{}
		p.files[name] = file
	}
	return file
}

func (p *downloadProgress) startTickerLocked() {
	if !p.enabled || p.stop != nil {
		return
	}
	p.stop = make(chan struct{})
	p.stopped = make(chan struct{})
	stop, stopped := p.stop, p.stopped
	go func() {
		ticker := time.NewTicker(progressRefreshInterval)
		defer ticker.Stop()
		defer close(stopped)
		for {
			select {
			case <-ticker.C:
				p.render()
			case <-stop:
				return
			}
		}
	}()
}

func (p *downloadProgress) render() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || !p.activeLocked() {
		return
	}
	p.renderLocked()
}

func (p *downloadProgress) renderLocked() {
	if !p.enabled || !p.sessionStarted || len(p.files) == 0 {
		return
	}

	var downloaded, total int64
	completed := 0
	unknownTotals := 0
	for _, file := range p.files {
		downloaded += file.downloaded
		if file.total > 0 {
			total += file.total
		} else {
			unknownTotals++
		}
		if file.finished && file.success {
			completed++
		}
	}

	percentKnown := unknownTotals == 0 && total > 0
	percent := 0.0
	if percentKnown {
		percent = float64(downloaded) * 100 / float64(total)
		if percent > 100 {
			percent = 100
		}
		if percent < 0 {
			percent = 0
		}
	}

	bar := strings.Repeat("-", progressBarWidth)
	if percentKnown {
		filled := int(percent * progressBarWidth / 100)
		if filled > progressBarWidth {
			filled = progressBarWidth
		}
		bar = strings.Repeat("#", filled) + strings.Repeat("-", progressBarWidth-filled)
	}

	seconds := time.Since(p.startedAt).Seconds()
	if seconds <= 0 {
		seconds = 1e-9
	}
	speed := float64(downloaded) / seconds
	line := fmt.Sprintf("Downloading [%s] %d/%d", bar, completed, len(p.files))
	if percentKnown {
		line += fmt.Sprintf(" %d%%", int(percent))
	}
	line += fmt.Sprintf(" %s", formatProgressBytes(downloaded))
	if percentKnown {
		line += fmt.Sprintf(" / %s", formatProgressBytes(total))
	}
	line += fmt.Sprintf(" %.1f MiB/s", speed/bytesPerMiB)

	// The line is erased before each redraw so shorter updates cannot leave
	// stale characters at the end of the previous line.
	_, _ = io.WriteString(p.output, "\r\033[2K"+line)
	p.lineRendered = true
}

func formatProgressBytes(value int64) string {
	return fmt.Sprintf("%.1f MiB", float64(value)/bytesPerMiB)
}

func (p *downloadProgress) activeLocked() bool {
	for _, file := range p.files {
		if file.started && !file.finished {
			return true
		}
	}
	return false
}

func (p *downloadProgress) clearIfIdleLocked() {
	if !p.activeLocked() {
		p.clearLocked()
	}
}

func (p *downloadProgress) clearLocked() {
	if !p.enabled || !p.lineRendered {
		return
	}
	_, _ = io.WriteString(p.output, "\r\033[2K")
	p.lineRendered = false
}

func (p *downloadProgress) close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	stop, stopped := p.stop, p.stopped
	p.clearLocked()
	if stop != nil {
		close(stop)
	}
	p.mu.Unlock()
	if stopped != nil {
		<-stopped
	}
}

type progressCountingReader struct {
	reader   io.Reader
	progress *downloadProgress
	name     string
}

func (r progressCountingReader) Read(data []byte) (int, error) {
	count, err := r.reader.Read(data)
	if count > 0 {
		r.progress.addBytes(r.name, int64(count))
	}
	return count, err
}
