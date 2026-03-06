package embed

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BatchClient handles async batch embedding requests to Voyage AI.
type BatchClient struct {
	apiKey   string
	model    string
	baseURL  string
	client   *http.Client
	stateDir string // Directory to persist batch state
}

// BatchState tracks the state of a batch embedding job.
type BatchState struct {
	BatchID      string    `json:"batch_id"`
	InputFileID  string    `json:"input_file_id"`
	OutputFileID string    `json:"output_file_id,omitempty"`
	ErrorFileID  string    `json:"error_file_id,omitempty"`
	Status       string    `json:"status"` // validating, in_progress, completed, failed, cancelled
	Total        int       `json:"total"`
	Completed    int       `json:"completed"`
	Failed       int       `json:"failed"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Model        string    `json:"model"`
}

// BatchRequest is a single request in the batch JSONL file.
// Voyage AI format: {"custom_id": "id", "body": {"input": "text"}}
type BatchRequest struct {
	CustomID string           `json:"custom_id"`
	Body     BatchRequestBody `json:"body"`
}

// BatchRequestBody contains the embedding request body.
// Note: input_type is not supported in batch API - only "input" field allowed.
type BatchRequestBody struct {
	Input string `json:"input"`
}

// BatchResponse is a single response from the batch output file.
// Voyage AI format: {"batch_id":"...", "custom_id":"...", "response": {"status_code": 200, "body": {...}}, "error": null}
type BatchResponse struct {
	BatchID  string         `json:"batch_id"`
	CustomID string         `json:"custom_id"`
	Response *BatchRespBody `json:"response"`
	Error    *BatchError    `json:"error,omitempty"`
}

// BatchRespBody contains the HTTP response from the batch API.
type BatchRespBody struct {
	StatusCode int             `json:"status_code"`
	Body       json.RawMessage `json:"body"`
}

// BatchError represents an error in batch processing.
type BatchError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// FileUploadResponse is the response from file upload.
type FileUploadResponse struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	Bytes     int64  `json:"bytes"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
	Filename  string `json:"filename"`
	Purpose   string `json:"purpose"`
}

// BatchCreateRequest is the request to create a batch.
type BatchCreateRequest struct {
	Endpoint         string            `json:"endpoint"`
	InputFileID      string            `json:"input_file_id"`
	CompletionWindow string            `json:"completion_window"`
	RequestParams    map[string]string `json:"request_params"`
}

// BatchCreateResponse is the response from batch creation.
type BatchCreateResponse struct {
	ID            string `json:"id"`
	Object        string `json:"object"`
	Endpoint      string `json:"endpoint"`
	InputFileID   string `json:"input_file_id"`
	OutputFileID  string `json:"output_file_id"`
	ErrorFileID   string `json:"error_file_id"`
	Status        string `json:"status"`
	RequestCounts struct {
		Total     int `json:"total"`
		Completed int `json:"completed"`
		Failed    int `json:"failed"`
	} `json:"request_counts"`
	CreatedAt string `json:"created_at"`
}

// NewBatchClient creates a new batch embedding client.
func NewBatchClient(stateDir string) (*BatchClient, error) {
	apiKey := os.Getenv("VOYAGE_API_KEY")
	model := os.Getenv("VOYAGE_MODEL")

	// Fall back to credentials file
	if apiKey == "" {
		creds, err := loadCredentials()
		if err != nil {
			return nil, fmt.Errorf("no API key: set VOYAGE_API_KEY or create ~/.config/snipe/credentials: %w", err)
		}
		if apiKey == "" {
			apiKey = creds["VOYAGE_API_KEY"]
		}
		if model == "" {
			model = creds["VOYAGE_MODEL"]
		}
	}

	if apiKey == "" {
		return nil, fmt.Errorf("VOYAGE_API_KEY not set")
	}
	if model == "" {
		model = "voyage-code-3"
	}

	return &BatchClient{
		apiKey:   apiKey,
		model:    model,
		baseURL:  "https://api.voyageai.com/v1",
		stateDir: stateDir,
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
	}, nil
}

// Model returns the model name being used.
func (c *BatchClient) Model() string {
	return c.model
}

// UploadFile uploads a JSONL file for batch processing.
// Uses io.Pipe to stream the multipart body without buffering the entire file in RAM.
func (c *BatchClient) UploadFile(jsonlPath string) (*FileUploadResponse, error) {
	file, err := os.Open(jsonlPath) // #nosec G304 -- path from caller (batch embedding JSONL)
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}
	defer file.Close()

	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)

	// Write multipart body in a goroutine so the pipe reader can stream to HTTP.
	go func() {
		part, err := writer.CreateFormFile("file", filepath.Base(jsonlPath))
		if err != nil {
			pw.CloseWithError(fmt.Errorf("create form file: %w", err))
			return
		}
		if _, err := io.Copy(part, file); err != nil {
			pw.CloseWithError(fmt.Errorf("copy file content: %w", err))
			return
		}
		if err := writer.WriteField("purpose", "batch"); err != nil {
			pw.CloseWithError(fmt.Errorf("write purpose field: %w", err))
			return
		}
		pw.CloseWithError(writer.Close())
	}()

	req, err := http.NewRequest("POST", c.baseURL+"/files", pr)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	var result FileUploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &result, nil
}

// CreateBatch creates a batch embedding job.
func (c *BatchClient) CreateBatch(inputFileID string) (*BatchCreateResponse, error) {
	reqBody := BatchCreateRequest{
		Endpoint:         "/v1/embeddings",
		InputFileID:      inputFileID,
		CompletionWindow: "12h",
		RequestParams:    map[string]string{"model": c.model},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", c.baseURL+"/batches", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	var result BatchCreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &result, nil
}

// GetBatchStatus retrieves the current status of a batch.
func (c *BatchClient) GetBatchStatus(batchID string) (*BatchCreateResponse, error) {
	req, err := http.NewRequest("GET", c.baseURL+"/batches/"+batchID, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	var result BatchCreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &result, nil
}

// DownloadFile downloads a file by ID and returns a streaming reader.
// Caller must close the returned ReadCloser.
// Uses a separate client that follows redirects (Voyage API returns redirect to S3).
func (c *BatchClient) DownloadFile(fileID string) (io.ReadCloser, error) {
	req, err := http.NewRequest("GET", c.baseURL+"/files/"+fileID+"/content", nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	// Use a client that follows redirects (default behavior)
	downloadClient := &http.Client{
		Timeout: 120 * time.Second,
	}

	resp, err := downloadClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	return resp.Body, nil
}

// ParseBatchResults streams JSONL from r, calling fn for each successfully parsed embedding.
// Stops early and returns the error if fn returns an error.
func (c *BatchClient) ParseBatchResults(r io.Reader, fn func(symbolID string, embedding []float32) error) error {
	scanner := bufio.NewScanner(r)

	// Handle potentially large lines
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}

		var resp BatchResponse
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			return fmt.Errorf("parse line: %w", err)
		}

		if resp.Error != nil {
			continue // Skip errors, they'll be in error file
		}

		if resp.Response == nil || resp.Response.StatusCode != 200 {
			continue // Skip non-200 responses
		}

		// Parse the embedding from response body
		var embResult EmbeddingResponse
		if err := json.Unmarshal(resp.Response.Body, &embResult); err != nil {
			return fmt.Errorf("parse embedding result for %s: %w", resp.CustomID, err)
		}

		if len(embResult.Data) > 0 {
			if err := fn(resp.CustomID, embResult.Data[0].Embedding); err != nil {
				return fmt.Errorf("process embedding for %s: %w", resp.CustomID, err)
			}
		}
	}

	return scanner.Err()
}

// SaveState persists the batch state to disk.
func (c *BatchClient) SaveState(state *BatchState) error {
	if c.stateDir == "" {
		return nil
	}

	if err := os.MkdirAll(c.stateDir, 0750); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	path := filepath.Join(c.stateDir, "batch_state.json")
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	return os.WriteFile(path, data, 0600)
}

// LoadState loads the batch state from disk.
func (c *BatchClient) LoadState() (*BatchState, error) {
	if c.stateDir == "" {
		return nil, nil
	}

	path := filepath.Join(c.stateDir, "batch_state.json")
	data, err := os.ReadFile(path) // #nosec G304 -- path derived from stateDir (batch state)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read state: %w", err)
	}

	var state BatchState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("unmarshal state: %w", err)
	}

	return &state, nil
}

// ClearState removes the batch state file.
func (c *BatchClient) ClearState() error {
	if c.stateDir == "" {
		return nil
	}

	path := filepath.Join(c.stateDir, "batch_state.json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove state: %w", err)
	}
	return nil
}

// WriteJSONL writes symbols to a JSONL file for batch processing.
// Returns the path to the created file.
func (c *BatchClient) WriteJSONL(symbols []SymbolText, outputDir string) (string, error) {
	if err := os.MkdirAll(outputDir, 0750); err != nil {
		return "", fmt.Errorf("create output dir: %w", err)
	}

	path := filepath.Join(outputDir, "embeddings.jsonl")
	file, err := os.Create(path) // #nosec G304 -- path derived from outputDir (batch JSONL)
	if err != nil {
		return "", fmt.Errorf("create file: %w", err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	for _, sym := range symbols {
		req := BatchRequest{
			CustomID: sym.ID,
			Body: BatchRequestBody{
				Input: sym.Text,
			},
		}
		if err := encoder.Encode(req); err != nil {
			return "", fmt.Errorf("encode request: %w", err)
		}
	}

	return path, nil
}

// SymbolText represents a symbol with its text for embedding.
type SymbolText struct {
	ID   string // Symbol ID (used as custom_id)
	Text string // Combined text (name + signature + doc)
}
