package gowarehousewave

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"sync"
)

// Journal 是事件日志抽象。Append 必须在事件对外生效前完成持久化；
// Replay 按写入顺序回放全部事件。
type Journal interface {
	Append(ev *Event) error
	Replay(handle func(*Event)) error
	io.Closer
}

// memoryJournal 仅保存在内存中，用于测试或不需要落盘的场景。
type memoryJournal struct {
	mu     sync.Mutex
	events []*Event
}

func newMemoryJournal() *memoryJournal { return &memoryJournal{} }

func (j *memoryJournal) Append(ev *Event) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	cp := *ev
	j.events = append(j.events, &cp)
	return nil
}

func (j *memoryJournal) Replay(handle func(*Event)) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, ev := range j.events {
		cp := *ev
		handle(&cp)
	}
	return nil
}

func (j *memoryJournal) Close() error { return nil }

// fileJournal 把事件以 JSON Lines 追加写入文件，每次追加 fsync。
// 整行 JSON 原子写出；若最后一行损坏（如写入中途掉电），
// Replay 会停在损坏行之前并返回错误，之前已完整落盘的事件全部可用。
type fileJournal struct {
	mu sync.Mutex
	f  *os.File
	w  *bufio.Writer
}

// openFileJournal 打开（不存在则创建）日志文件并定位到末尾。
func openFileJournal(path string) (*fileJournal, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &fileJournal{f: f, w: bufio.NewWriter(f)}, nil
}

func (j *fileJournal) Append(ev *Event) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := j.w.Write(data); err != nil {
		return err
	}
	if err := j.w.Flush(); err != nil {
		return err
	}
	return j.f.Sync()
}

func (j *fileJournal) Replay(handle func(*Event)) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, err := j.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	defer j.f.Seek(0, io.SeekEnd)
	sc := bufio.NewScanner(j.f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return err
		}
		handle(&ev)
	}
	return sc.Err()
}

func (j *fileJournal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.w.Flush(); err != nil {
		j.f.Close()
		return err
	}
	return j.f.Close()
}
