package iop

import (
	"bufio"
	"context"
	"io"
	"os"
	"path"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/flarco/g"
	"github.com/flarco/g/json"
	cmap "github.com/orcaman/concurrent-map/v2"
	"github.com/slingdata-io/sling-cli/core/env"
)

type Queue struct {
	Name    string        `json:"name"`
	Path    string        `json:"path"`
	File    *os.File      `json:"-"`
	Reader  *bufio.Reader `json:"-"`
	Writer  *bufio.Writer `json:"-"`
	mu      sync.Mutex    // protect concurrent access
	count   int
	reading bool // whether queue is in reading mode
	writing bool // whether queue is in writing mode
	keep    bool
}

// donePath is the sentinel file (next to the queue file, so it works across
// processes) signaling the producer finished writing. Tailing readers watch it.
func (q *Queue) donePath() string {
	return q.Path + ".done"
}

// MarkDone writes the done-sentinel; call after the producer flushed all writes.
func (q *Queue) MarkDone() error {
	f, err := os.OpenFile(q.donePath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return g.Error(err, "could not write queue done-sentinel: %s", q.donePath())
	}
	return f.Close()
}

// IsDone reports whether the producer has signaled completion.
func (q *Queue) IsDone() bool {
	return g.PathExists(q.donePath())
}

var queues = cmap.New[*Queue]()

// NewQueue creates a new queue with a temporary file
func NewQueue(name string) (q *Queue, err error) {
	// make temp folder
	var tmpFile *os.File
	var keep bool

	if env.IsThreadChild {
		folder := env.QueueFolder()
		keep = true // keep file, will be deleted by parent process
		if err := os.MkdirAll(folder, 0755); err != nil {
			return nil, g.Error(err, "could not create queue folder")
		}
		tmpFilePath := path.Join(folder, name+".queue")
	retry:
		if g.PathExists(tmpFilePath) {
			if tmpFile, err = os.OpenFile(tmpFilePath, os.O_RDWR, 0755); err != nil {
				return nil, g.Error(err, "could not open queue file: %s", tmpFilePath)
			}
		} else {
			if tmpFile, err = os.OpenFile(tmpFilePath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0755); err != nil {
				if strings.Contains(err.Error(), "file exists") {
					goto retry // race condition when using SLING_THREADS
				}
				return nil, g.Error(err, "could not create queue file: %s", tmpFilePath)
			}
		}
	} else {
		tmpFile, err = os.CreateTemp("", name+"_*.queue")
		if err != nil {
			return nil, g.Error(err, "failed to create temp file for queue")
		}
	}

	q = &Queue{
		Name:    name,
		Path:    tmpFile.Name(),
		File:    tmpFile,
		writing: true, // start in writing mode
		reading: false,
		keep:    keep,
	}

	q.Writer = bufio.NewWriter(tmpFile)

	g.Trace("using queue `%s` at %s", name, q.Path)

	queues.Set(name, q)
	return q, nil
}

// Append writes a line to the queue
func (q *Queue) Append(data any) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Cannot write if in reading mode
	if q.reading {
		return g.Error("queue is in reading mode, cannot write")
	}

	// Ensure we're in writing mode
	if !q.writing {
		if err := q.startWriting(); err != nil {
			return g.Error(err, "failed to start writing mode")
		}
	}

	if items, ok := explodeItems(data); ok {
		if len(items) == 0 {
			return nil
		}
		for _, item := range items {
			if err := q.writeItem(item); err != nil {
				return err
			}
		}
		return nil
	}

	return q.writeItem(data)
}

func (q *Queue) writeItem(data any) error {
	// Always JSON encode the data to handle special characters and complex types properly
	encoded := g.Marshal(data)

	// Add a newline for record separation
	if !strings.HasSuffix(encoded, "\n") {
		encoded += "\n"
	}

	_, err := q.Writer.WriteString(encoded)
	if err != nil {
		return g.Error(err, "failed to write to queue")
	}
	q.count++

	// Flush after each write to ensure data is written to disk immediately
	return q.Writer.Flush()
}

func explodeItems(data any) ([]any, bool) {
	if data == nil {
		return nil, false
	}

	switch data.(type) {
	case []byte:
		return nil, false
	}

	val := reflect.ValueOf(data)
	if !val.IsValid() {
		return nil, false
	}

	for val.Kind() == reflect.Pointer {
		if val.IsNil() {
			return nil, false
		}
		val = val.Elem()
	}

	kind := val.Kind()
	if kind != reflect.Slice && kind != reflect.Array {
		return nil, false
	}

	if kind == reflect.Slice && val.Type().Elem().Kind() == reflect.Uint8 {
		return nil, false
	}

	length := val.Len()
	items := make([]any, length)
	for i := 0; i < length; i++ {
		items[i] = val.Index(i).Interface()
	}
	return items, true
}

// Flush flushes and syncs buffered writes to disk; call before MarkDone.
func (q *Queue) Flush() error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.Writer != nil {
		if err := q.Writer.Flush(); err != nil {
			return g.Error(err, "failed to flush queue writer")
		}
	}
	if q.File != nil && q.writing {
		if err := q.File.Sync(); err != nil {
			return g.Error(err, "failed to sync queue file")
		}
	}
	return nil
}

// startWriting prepares the queue for writing
func (q *Queue) startWriting() error {
	// Cannot start writing if in reading mode
	if q.reading {
		return g.Error("queue is in reading mode, cannot switch to writing mode")
	}

	// Close existing file if open
	if q.File != nil {
		if q.Writer != nil {
			q.Writer.Flush() // Ensure data is flushed before closing
		}
		q.Writer = nil

		if err := q.File.Close(); err != nil {
			return g.Error(err, "failed to close file before starting writing")
		}
		q.File = nil
	}

	// Open file for writing (append mode)
	file, err := os.OpenFile(q.Path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return g.Error(err, "failed to open file for writing")
	}

	q.File = file
	q.Writer = bufio.NewWriter(file)
	q.writing = true

	return nil
}

// finishWriting completes the writing phase
func (q *Queue) finishWriting() error {
	if !q.writing {
		return nil // Already finished writing
	}

	if q.Writer != nil {
		if err := q.Writer.Flush(); err != nil {
			return g.Error(err, "failed to flush writer")
		}
		q.Writer = nil
	}

	if q.File != nil {
		// Make sure all data is synced to disk
		if err := q.File.Sync(); err != nil {
			return g.Error(err, "failed to sync file")
		}

		if err := q.File.Close(); err != nil {
			return g.Error(err, "failed to close file after writing")
		}
		q.File = nil
	}

	q.writing = false
	return nil
}

// startReading prepares the queue for reading
func (q *Queue) startReading() error {
	// Finish writing phase if active
	if q.writing {
		if err := q.finishWriting(); err != nil {
			return g.Error(err, "failed to finish writing before reading")
		}
	}

	// Close existing file if open
	if q.File != nil {
		q.Reader = nil
		if err := q.File.Close(); err != nil {
			return g.Error(err, "failed to close file before starting reading")
		}
		q.File = nil
	}

	// Open file for reading
	file, err := os.OpenFile(q.Path, os.O_RDONLY, 0644)
	if err != nil {
		return g.Error(err, "failed to open file for reading")
	}

	q.File = file
	q.Reader = bufio.NewReader(file)
	q.reading = true
	q.writing = false // Cannot write after reading starts

	return nil
}

// Next reads the next line from the queue
// Returns the line, a boolean indicating if there are more lines, and any error
func (q *Queue) Next() (any, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Ensure we're in reading mode
	if !q.reading {
		if err := q.startReading(); err != nil {
			return nil, false, g.Error(err, "failed to start reading mode")
		}
	}

	// Read the next line
	line, err := q.Reader.ReadString('\n')
	if err != nil {
		if err == io.EOF {
			// Check if there's any content before EOF
			if len(line) > 0 {
				// Process the last line without newline
				return decodeJSONLine(line), true, nil
			}
			return nil, false, nil
		}
		return nil, false, g.Error(err, "failed to read from queue")
	}

	// Remove trailing newline and decode
	return decodeJSONLine(line), true, nil
}

// decodeJSONLine processes a JSON encoded line from the queue
func decodeJSONLine(line string) any {
	// Remove trailing newline
	line = strings.TrimSuffix(line, "\n")

	// Decode the JSON
	var result any
	if err := json.Unmarshal([]byte(line), &result); err != nil {
		// If not valid JSON, return as string
		g.Debug("Failed to decode JSON: %v, raw: %s", err, line)
		return line
	}
	return result
}

// Reset positions the reader at the beginning of the file
func (q *Queue) Reset() error {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Ensure we're in reading mode
	if !q.reading {
		if err := q.startReading(); err != nil {
			return g.Error(err, "failed to start reading mode")
		}
	}

	// Seek to beginning of file
	_, err := q.File.Seek(0, io.SeekStart)
	if err != nil {
		return g.Error(err, "failed to reset queue position")
	}

	// Re-initialize the reader
	q.Reader = bufio.NewReader(q.File)
	return nil
}

// Close closes and optionally removes the queue file
func (q *Queue) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Clean up resources
	if q.Writer != nil {
		q.Writer.Flush()
		q.Writer = nil
	}

	if q.Reader != nil {
		q.Reader = nil
	}

	if q.File != nil {
		path := q.File.Name()

		// Close the file
		if err := q.File.Close(); err != nil {
			return err
		}
		q.File = nil

		// Remove the file
		if !q.keep {
			if err := os.Remove(path); err != nil {
				return err
			}
			os.Remove(q.donePath()) // best-effort sentinel cleanup
		}

		q.reading = false
		q.writing = false

	}

	return nil
}

func CloseQueues() {
	for _, q := range queues.Items() {
		q.Close()
	}
}

// QueuePollInterval is how often a tailing reader re-checks the file at EOF.
var QueuePollInterval = 50 * time.Millisecond

// QueueReader is an independent tailing reader over a Queue file. Each consumer
// gets its own fd and offset, so all consumers see every record (broadcast).
// Polls for growth + watches the done-sentinel (no OS file-watch); cross-platform.
type QueueReader struct {
	queue  *Queue
	file   *os.File
	reader *bufio.Reader
}

// NewReader opens a fresh tailing reader at the start of the file; caller Closes it.
func (q *Queue) NewReader() (*QueueReader, error) {
	// flush our writer (in-process); cross-process the producer flushes its own
	q.mu.Lock()
	if q.Writer != nil {
		q.Writer.Flush()
	}
	q.mu.Unlock()

	// read-only; Go opens with shared access on Windows, so concurrent append is fine
	file, err := os.OpenFile(q.Path, os.O_RDONLY, 0644)
	if err != nil {
		return nil, g.Error(err, "could not open queue file for tailing: %s", q.Path)
	}

	return &QueueReader{
		queue:  q,
		file:   file,
		reader: bufio.NewReader(file),
	}, nil
}

// Next returns the next record, blocking (polling) at EOF until the producer
// appends more or signals done. Returns hasMore=false when done and drained, or
// when ctx is cancelled (so a failing sibling unblocks all readers promptly).
func (qr *QueueReader) Next(ctx context.Context) (data any, hasMore bool, err error) {
	for {
		line, readErr := qr.reader.ReadString('\n')

		if readErr == nil {
			return decodeJSONLine(line), true, nil
		}

		if readErr != io.EOF {
			return nil, false, g.Error(readErr, "failed to read from queue: %s", qr.queue.Path)
		}

		// EOF with a partial (no-newline) line: emit it if done, else wait for the newline
		if len(line) > 0 {
			if qr.queue.IsDone() {
				return decodeJSONLine(line), true, nil
			}
			if seekErr := qr.rewind(int64(len(line))); seekErr != nil {
				return nil, false, seekErr
			}
		} else if qr.queue.IsDone() {
			return nil, false, nil // clean EOF and producer done
		}

		select {
		case <-ctx.Done():
			return nil, false, nil
		case <-time.After(QueuePollInterval):
		}

		// re-create reader so it picks up newly-appended bytes (bufio caches EOF)
		qr.reader = bufio.NewReader(qr.file)
	}
}

// rewind moves the file offset back n bytes to re-read a partial trailing line.
func (qr *QueueReader) rewind(n int64) error {
	if _, err := qr.file.Seek(-n, io.SeekCurrent); err != nil {
		return g.Error(err, "failed to rewind queue reader: %s", qr.queue.Path)
	}
	qr.reader = bufio.NewReader(qr.file)
	return nil
}

// Close releases the reader's file descriptor.
func (qr *QueueReader) Close() error {
	if qr.file != nil {
		err := qr.file.Close()
		qr.file = nil
		qr.reader = nil
		return err
	}
	return nil
}
