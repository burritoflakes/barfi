package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/rs/zerolog/log"
)

type Uploader struct {
	Reader         *ProgressReader
	Size           int64
	DirectoryId    *string
	Token          *string
	Name           string
	Error          string
	UploadId       string
	UploadUrls     []string
	CompletedParts []Part
	ChunkSize      int64
}

type Part struct {
	ETag       string
	PartNumber int
}

func NewUploader(file *os.File, directoryId, token *string) (*Uploader, error) {

	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}

	if stat.Size() == 0 {
		return nil, fmt.Errorf("invalid file with size 0")
	}

	if directoryId != nil && token == nil {
		return nil, fmt.Errorf("when directoryId is supplied, token is required")
	}

	log.Info().Str("name", stat.Name()).Int64("size", stat.Size()).Msg("init upload")

	progressCallback := func(read int64, size int64) {
		if !*silentMode {
			fmt.Printf("\rtotal upload: %s/%s    progress: %d%%", Hrs(read), Hrs(size), int(float32(read*100)/float32(size)))
		}
	}

	reader := NewProgressReader(file, stat.Size(), progressCallback)

	return &Uploader{
		Reader:         reader,
		Size:           stat.Size(),
		Name:           stat.Name(),
		DirectoryId:    directoryId,
		Token:          token,
		UploadId:       "",
		UploadUrls:     []string{},
		CompletedParts: []Part{},
		ChunkSize:      5 * 1024 * 1024 * 1024, // 5GB
	}, nil
}

func (u *Uploader) Upload() error {
	if err := u.init(); err != nil {
		return err
	}

	chunkCount := (u.Size + u.ChunkSize - 1) / u.ChunkSize

	for i := int64(0); i < chunkCount; i++ {
		start := i * u.ChunkSize
		end := start + u.ChunkSize
		if end > u.Size {
			end = u.Size
		}

		part, err := u.uploadChunk(start, end, u.UploadUrls[i], int(i+1))
		if err != nil {
			return err
		}
		u.CompletedParts = append(u.CompletedParts, *part)
	}

	id, err := u.complete()
	if err != nil {
		return err
	}

	fmt.Printf("\n%s/f/%s", endpoint, id)

	return nil
}

func (u *Uploader) uploadChunk(start int64, end int64, signedUrl string, partNumber int) (*Part, error) {
	_, err := u.Reader.Seek(start, 0)
	if err != nil {
		return nil, fmt.Errorf("file seek error: %s", err)
	}

	log.Debug().Str("url", signedUrl).Int64("start", start).Int64("end", end).Int("part", partNumber).Msg("uploading chunk")

	client := &http.Client{}
	req, err := http.NewRequest("PUT", signedUrl, io.NewSectionReader(u.Reader, start, end-start))
	if err != nil {
		return nil, err
	}
	req.ContentLength = end - start

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		etag := resp.Header.Get("ETag")
		return &Part{ETag: etag, PartNumber: partNumber}, nil
	} else {
		return nil, fmt.Errorf("upload failed with status code %d", resp.StatusCode)
	}
}

func (u *Uploader) init() error {
	requestBody, err := json.Marshal(map[string]interface{}{
		"name": u.Name,
		"size": u.Size,
	})
	if err != nil {
		return err
	}

	client := &http.Client{}
	req, err := http.NewRequest("POST", endpoint+"/f/", bytes.NewReader(requestBody))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	if u.Token != nil {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", *u.Token))
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("init upload failed with status code %d", resp.StatusCode)
	}

	var data struct {
		UploadId   string   `json:"uploadId"`
		UploadUrls []string `json:"uploadUrls"`
	}

	err = json.NewDecoder(resp.Body).Decode(&data)
	if err != nil {
		return err
	}

	log.Debug().Str("uploadId", data.UploadId).Any("uploadUrls", data.UploadUrls).Msg("retrieved uploadId")

	u.UploadId = data.UploadId
	u.UploadUrls = data.UploadUrls
	return nil
}

func (u *Uploader) complete() (string, error) {
	requestBody, err := json.Marshal(map[string]interface{}{
		"directoryId": u.DirectoryId,
		"parts":       u.CompletedParts,
	})
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("%s/f/%s", endpoint, u.UploadId)
	if u.DirectoryId != nil {
		url = fmt.Sprintf("%s?directoryId=%s", url, *u.DirectoryId)
	}

	client := &http.Client{}
	req, err := http.NewRequest("POST", fmt.Sprintf("%s/f/%s", endpoint, u.UploadId), bytes.NewReader(requestBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	clog := log.Debug().Str("url", url)

	if u.Token != nil {
		clog.Str("token", *token)
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", *u.Token))
	}

	clog.Msg("sending complete upload request")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("complete upload failed with status code %d", resp.StatusCode)
	}

	var data struct {
		Id string `json:"id"`
	}

	err = json.NewDecoder(resp.Body).Decode(&data)
	if err != nil {
		return "", err
	}

	return data.Id, err
}
