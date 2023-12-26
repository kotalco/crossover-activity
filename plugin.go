package crossover

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
	DefaultTimeout           = 10
	MaxRequestBodySize int64 = 2 * 1024 * 1024 // 2 MB
)

// Config holds configuration to passed to the plugin
type Config struct {
	Pattern       string
	RemoteAddress string
	APIKey        string
}

// CreateConfig populates the config data object
func CreateConfig() *Config {
	return &Config{}
}

type RequestLogger struct {
	next            http.Handler
	name            string
	client          *http.Client
	compiledPattern *regexp.Regexp
	remoteAddress   string
	apiKey          string
}

// loggingRequestDto used to send request to the third party to save no of requests
type loggingRequestDto struct {
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

	client := &http.Client{
		Timeout: DefaultTimeout * time.Second,
	}
	compiledPattern := regexp.MustCompile(config.Pattern)

	return &RequestLogger{
		next:            next,
		name:            name,
		client:          client,
		compiledPattern: compiledPattern,
		remoteAddress:   config.RemoteAddress,
		apiKey:          config.APIKey,
	}, nil
}

func (a *RequestLogger) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufferPool.Put(buf)

	// Limit the size of the request body that we will read
	//this will guard the plugin malicious body request by users
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

	go a.log(clonedRequest)

	a.next.ServeHTTP(rw, req)
}

func (a *RequestLogger) log(req *http.Request) error {
	requestId := a.requestKey(req.URL.Path)
	requestBody := loggingRequestDto{RequestId: requestId, Count: requestCount(req)}

	// Get a buffer from the pool and reset it back
	buffer := bufferPool.Get().(*bytes.Buffer)
	buffer.Reset()
	defer bufferPool.Put(buffer)
	encoder := json.NewEncoder(buffer)
	err := encoder.Encode(requestBody)
	if err != nil {
		return err
	}

	httpReq, err := http.NewRequest(http.MethodPost, a.remoteAddress, buffer)
	if err != nil {
		log.Printf("HTTPCALLERERR: %s", err.Error())
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Api-Key", a.apiKey)

	httpRes, err := a.client.Do(httpReq)
	if err != nil {
		log.Printf("HTTPDOERR: %s", err.Error())
		return err
	}
	defer httpRes.Body.Close()

	if httpRes.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(httpRes.Body)
		return fmt.Errorf("unexpected status code: %d, body: %s", httpRes.StatusCode, string(bodyBytes))
	}
	return nil
}

func (a *RequestLogger) requestKey(path string) string {
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
	if err := decoder.Decode(&requests); err != nil {
		log.Printf("REQEUST_COUNT_ERR: %s", err.Error())
		return 0
	}

	// ensure that it is fully read to the end and then closed to avoid resource leaks
	io.Copy(io.Discard, req.Body)
	req.Body.Close()

	count = len(requests)
	return count
}
