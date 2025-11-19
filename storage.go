// File: storage.go
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"cloud.google.com/go/storage"
)

// FileInfo represents uploaded file metadata
type FileInfo struct {
	Path string `json:"path"` // GCS path (gs://bucket/path)
	Name string `json:"name"` // Original filename
	Size int64  `json:"size"` // File size in bytes
	Type string `json:"type"` // MIME type
}

// UploadResult contains upload operation results
type UploadResult struct {
	Successful []FileInfo    `json:"successful"`
	Failed     []UploadError `json:"failed"`
}

// UploadError represents a failed file upload
type UploadError struct {
	File  string `json:"file"`
	Error string `json:"error"`
}

// InitStorageClient creates GCS client using ADC (Application Default Credentials)
func InitStorageClient(ctx context.Context) (*storage.Client, error) {
	return storage.NewClient(ctx)
}

// UploadFile uploads a single file to GCS with workspace isolation
func UploadFile(
	ctx context.Context,
	client *storage.Client,
	bucketName string,
	localPath string,
	workspaceID string,
	executionID string,
	log *slog.Logger,
) (FileInfo, error) {
	// Open local file
	file, err := os.Open(localPath)
	if err != nil {
		return FileInfo{}, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	// Get file info
	stat, err := file.Stat()
	if err != nil {
		return FileInfo{}, fmt.Errorf("failed to stat file: %w", err)
	}

	// Build GCS path with workspace isolation
	filename := filepath.Base(localPath)
	gcsPath := fmt.Sprintf(
		"private/workspace/%s/terminal-uploads/%s/%s",
		workspaceID,
		executionID,
		filename,
	)

	log.Info("Uploading file to GCS",
		"file", filename,
		"size", stat.Size(),
		"gcsPath", gcsPath)

	// Upload to GCS
	bucket := client.Bucket(bucketName)
	obj := bucket.Object(gcsPath)
	writer := obj.NewWriter(ctx)

	// Set metadata
	writer.ContentType = detectContentType(filename)
	writer.Metadata = map[string]string{
		"uploaded-by":   "aiexpedite-local-terminal",
		"workspace-id":  workspaceID,
		"execution-id":  executionID,
		"original-name": filename,
	}

	// Copy file data
	if _, err := io.Copy(writer, file); err != nil {
		writer.Close()
		return FileInfo{}, fmt.Errorf("failed to upload: %w", err)
	}

	if err := writer.Close(); err != nil {
		return FileInfo{}, fmt.Errorf("failed to finalize upload: %w", err)
	}

	log.Info("File uploaded successfully", "file", filename, "gcsPath", gcsPath)

	return FileInfo{
		Path: fmt.Sprintf("gs://%s/%s", bucketName, gcsPath),
		Name: filename,
		Size: stat.Size(),
		Type: writer.ContentType,
	}, nil
}

// UploadFiles uploads multiple files concurrently with error collection
func UploadFiles(
	ctx context.Context,
	client *storage.Client,
	bucketName string,
	localPaths []string,
	workspaceID string,
	executionID string,
	log *slog.Logger,
) UploadResult {
	result := UploadResult{
		Successful: make([]FileInfo, 0),
		Failed:     make([]UploadError, 0),
	}

	if len(localPaths) == 0 {
		return result
	}

	log.Info("Starting batch file upload",
		"fileCount", len(localPaths),
		"workspaceID", workspaceID,
		"executionID", executionID)

	// Upload files concurrently (max 10 at a time)
	semaphore := make(chan struct{}, 10)
	resultChan := make(chan interface{}, len(localPaths))
	var wg sync.WaitGroup

	for _, localPath := range localPaths {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()

			semaphore <- struct{}{}        // Acquire
			defer func() { <-semaphore }() // Release

			fileInfo, err := UploadFile(ctx, client, bucketName, path, workspaceID, executionID, log)
			if err != nil {
				resultChan <- UploadError{File: path, Error: err.Error()}
			} else {
				resultChan <- fileInfo
			}
		}(localPath)
	}

	// Wait for all uploads to complete
	go func() {
		wg.Wait()
		close(resultChan)
	}()

	// Collect results
	for res := range resultChan {
		switch v := res.(type) {
		case FileInfo:
			result.Successful = append(result.Successful, v)
		case UploadError:
			result.Failed = append(result.Failed, v)
		}
	}

	log.Info("Batch file upload complete",
		"successful", len(result.Successful),
		"failed", len(result.Failed))

	return result
}

// detectContentType returns MIME type based on file extension
func detectContentType(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	case ".webm":
		return "video/webm"
	case ".mp4":
		return "video/mp4"
	case ".mov":
		return "video/quicktime"
	case ".json":
		return "application/json"
	case ".html", ".htm":
		return "text/html"
	case ".txt":
		return "text/plain"
	case ".pdf":
		return "application/pdf"
	case ".zip":
		return "application/zip"
	case ".tar":
		return "application/x-tar"
	case ".gz":
		return "application/gzip"
	default:
		return "application/octet-stream"
	}
}
