package crossover_activity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"sync"
	"time"
)

const (
	DefaultTimeout                  = 10
	MaxRequestBodySize        int64 = 2 * 1024 * 1024 // 2 MB
	DefaultLogBufferSize            = 100000          // buffer size for the log entries channel
	DefaultMaxBatchSize             = 20              // number of activity to batch together
	DefaultBatchFlushInterval       = 2               // Time interval to flush logs to the database
)

// Config holds configuration to passed to the plugin
type Config struct {
	Pattern       string
	RemoteAddress string
	APIKey        string
	BufferSize    int
	BatchSize     int
	FlushInterval int
}

// CreateConfig populates the config data object
func CreateConfig() *Config {
	return &Config{}
}

type Activity struct {
	logsChannel     chan activityRequestDto
	next            http.Handler
	name            string
	client          *http.Client
	compiledPattern *regexp.Regexp
	remoteAddress   string
	apiKey          string
	batchSize       int
	flushInterval   int
}

// loggingRequestDto used to send request to the third party to save no of requests
type activityRequestDto struct {
	RequestId string `json:"request_id"`
	Count     int    `json:"count"`
}

// implement buffer pool using the sync.Pool type,to reduce the allocation when you are encoding JSON
var bufferPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

// New created a new  plugin.
func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	if len(config.APIKey) == 0 {
		return nil, fmt.Errorf("APIKey can't be empty")
	}
	if len(config.Pattern) == 0 {
		return nil, fmt.Errorf("pattern can't be empty")
	}
	if len(config.RemoteAddress) == 0 {
		return nil, fmt.Errorf("RemoteAddress can't be empty")
	}
	if config.BufferSize == 0 {
		config.BufferSize = DefaultLogBufferSize
	}
	if config.BatchSize == 0 {
		config.BatchSize = DefaultMaxBatchSize
	}
	if config.FlushInterval == 0 {
		config.FlushInterval = DefaultBatchFlushInterval
	}

	client := &http.Client{
		Timeout: DefaultTimeout * time.Second,
	}
	compiledPattern := regexp.MustCompile(config.Pattern)

	handler := &Activity{
		logsChannel:     make(chan activityRequestDto, config.BufferSize),
		next:            next,
		name:            name,
		client:          client,
		compiledPattern: compiledPattern,
		remoteAddress:   config.RemoteAddress,
		apiKey:          config.APIKey,
		batchSize:       config.BatchSize,
		flushInterval:   config.FlushInterval,
	}
	go handler.batchProcessor()
	return handler, nil
}

func (a *Activity) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufferPool.Put(buf)

	// Limit the size of the request body that we will read
	//this will guard the plugin from malicious body request by users
	_, err := io.CopyN(buf, req.Body, MaxRequestBodySize)
	req.Body.Close()
	if err != nil && err != io.EOF {
		log.Printf("Error reading request body: %s", err)
		http.Error(rw, "Error reading request body", http.StatusInternalServerError)
		return
	}

	bodyReader := bytes.NewReader(buf.Bytes())
	req.Body = io.NopCloser(bodyReader)
	clonedRequest := req.Clone(req.Context())
	clonedRequest.Body = io.NopCloser(bytes.NewReader(buf.Bytes()))

	// Create log entry
	logEntry := activityRequestDto{
		RequestId: a.requestKey(clonedRequest.URL.Path),
		Count:     requestCount(clonedRequest),
	}

	//send logEntry to logsChannel with select and don't block
	select {
	case a.logsChannel <- logEntry:
	default:
		log.Printf("Dropped some log entries due to full buffer channel")
	}

	a.next.ServeHTTP(rw, req)
}

// batchProcessor runs in a separate goroutine and batches logs.
func (a *Activity) batchProcessor() {
	var batch []activityRequestDto
	flushTimer := time.NewTimer(time.Duration(a.flushInterval) * time.Second)
	for {
		select {
		case logEntry := <-a.logsChannel:
			batch = append(batch, logEntry)
			if len(batch) >= a.batchSize {
				a.flushLogs(batch)
				batch = nil // clear the batch
			}
		case <-flushTimer.C:
			if len(batch) > 0 {
				a.flushLogs(batch)
				batch = nil // clear the batch
			}
			flushTimer.Reset(time.Duration(a.flushInterval) * time.Second)
		}
	}
}

// flushLogs sends a batch of logs to the database.
func (a *Activity) flushLogs(batch []activityRequestDto) {
	// Aggregate the data and send it to the database in batches
	// Get a buffer from the pool and reset it back
	buffer := bufferPool.Get().(*bytes.Buffer)
	buffer.Reset()
	defer bufferPool.Put(buffer)

	encoder := json.NewEncoder(buffer)
	err := encoder.Encode(batch)
	if err != nil {
		log.Printf("FLUSH_LOGS: %s", err.Error())
		return
	}
	httpReq, err := http.NewRequest(http.MethodPost, a.remoteAddress, buffer)
	if err != nil {
		log.Printf("FLUSH_LOGS: %s", err.Error())
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Api-Key", a.apiKey)

	httpRes, err := a.client.Do(httpReq)
	defer httpRes.Body.Close()
	if err != nil {
		log.Printf("FLUSH_LOGS: %s", err.Error())
		return
	}

	if httpRes.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(httpRes.Body)
		log.Printf("unexpected status code: %d, body: %s", httpRes.StatusCode, string(bodyBytes))
		return
	}

}

func (a *Activity) requestKey(path string) string {
	match := a.compiledPattern.FindStringSubmatch(path)
	if len(match) == 0 {
		return ""
	}
	return match[0]
}

func requestCount(req *http.Request) (count int) {
	contentType := req.Header.Get("Content-Type")
	if contentType != "application/json" {
		// if it's not of type json default to 1 and return before reading the body
		return 1
	}

	decoder := json.NewDecoder(req.Body)
	var requests []interface{}
	err := decoder.Decode(&requests)

	io.Copy(io.Discard, req.Body)
	req.Body.Close()

	if err != nil {
		//if it fails to decode []objects assume it's a single object then return
		return 1
	}
	count = len(requests)
	return count
}
