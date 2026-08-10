package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type objectStorageConfig struct {
	endpoint string
	bucket   string
	access   string
	secret   string
	secure   bool
}

func loadObjectStorageConfig() (objectStorageConfig, error) {
	cfg := objectStorageConfig{
		endpoint: strings.TrimSpace(os.Getenv("S3_ENDPOINT")),
		bucket:   strings.TrimSpace(os.Getenv("S3_BUCKET")),
		access:   strings.TrimSpace(os.Getenv("S3_ACCESS_KEY")),
		secret:   strings.TrimSpace(os.Getenv("S3_SECRET_KEY")),
	}
	if cfg.endpoint == "" || cfg.bucket == "" || cfg.access == "" || cfg.secret == "" {
		return cfg, fmt.Errorf("object storage is not configured: set S3_ENDPOINT, S3_BUCKET, S3_ACCESS_KEY, S3_SECRET_KEY")
	}

	u, err := url.Parse(cfg.endpoint)
	if err == nil && u.Host != "" {
		cfg.endpoint = u.Host
		cfg.secure = u.Scheme == "https"
		return cfg, nil
	}

	// Bare host:port without scheme defaults to HTTP for local MinIO.
	cfg.secure = false
	return cfg, nil
}

func newObjectStorageClient() (*minio.Client, objectStorageConfig, error) {
	cfg, err := loadObjectStorageConfig()
	if err != nil {
		return nil, cfg, err
	}
	client, err := minio.New(cfg.endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.access, cfg.secret, ""),
		Secure: cfg.secure,
	})
	if err != nil {
		return nil, cfg, err
	}
	return client, cfg, nil
}

func uploadInvoicePhoto(invoiceID int64, filename, contentType string, content []byte) (string, error) {
	client, cfg, err := newObjectStorageClient()
	if err != nil {
		return "", err
	}
	key := path.Join("invoices", fmt.Sprintf("%d", invoiceID), fmt.Sprintf("%d_%s", time.Now().UnixNano(), sanitizeFilename(filename)))
	_, err = client.PutObject(
		context.Background(),
		cfg.bucket,
		key,
		bytes.NewReader(content),
		int64(len(content)),
		minio.PutObjectOptions{ContentType: contentType},
	)
	if err != nil {
		return "", err
	}
	return key, nil
}

func openInvoicePhotoObject(objectKey string) (io.ReadCloser, string, error) {
	client, cfg, err := newObjectStorageClient()
	if err != nil {
		return nil, "", err
	}
	obj, err := client.GetObject(context.Background(), cfg.bucket, objectKey, minio.GetObjectOptions{})
	if err != nil {
		return nil, "", err
	}
	st, err := obj.Stat()
	if err != nil {
		obj.Close()
		return nil, "", err
	}
	return obj, st.ContentType, nil
}

func deleteInvoicePhotoObject(objectKey string) error {
	client, cfg, err := newObjectStorageClient()
	if err != nil {
		return err
	}
	return client.RemoveObject(context.Background(), cfg.bucket, objectKey, minio.RemoveObjectOptions{})
}

func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "photo.jpg"
	}
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "*", "_", "?", "_", "\"", "_", "<", "_", ">", "_", "|", "_")
	name = replacer.Replace(name)
	name = strings.Join(strings.Fields(name), "_")
	if name == "" {
		return "photo.jpg"
	}
	return name
}
