package main

import (
	"cloud.google.com/go/storage"
	vision "cloud.google.com/go/vision/apiv1"
	"flag"
	"github.com/tj/go-spin"
	visionpb "google.golang.org/genproto/googleapis/cloud/vision/v1"
	"io"
	"strconv"
	"time"

	"context"
	"fmt"
	"os"
)

var bucket string

func main() {
	err := run()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	flag.StringVar(&bucket, "bucket", "", "GCS bucket")
	flag.Parse()
	filename := flag.Arg(1)

	if filename == "" {
		return fmt.Errorf("invalid argument: filename must specified")
	}

	if bucket == "" {
		return fmt.Errorf("invalid argument: please specify -bucket flag")
	}

	ctx := context.Background()
	file, err := os.Open(filename)
	if err != nil {
		return err
	}

	endOfGCS := waitingMessage("waiting for GCS")
	name, err := uploadPDF(ctx, file)
	if err != nil {
		return err
	}
	endOfGCS()

	endOfOCR := waitingMessage("waiting for OCR")
	err = ocrPDF(ctx, name)
	if err != nil {
		return err
	}
	endOfOCR()

	return nil
}

func uploadPDF(ctx context.Context, r io.Reader) (string, error) {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return "", err
	}

	name := strconv.FormatInt(time.Now().UnixNano(), 10)

	object := client.Bucket(bucket).Object(name)
	writer := object.NewWriter(ctx)
	defer writer.Close()

	_, err = io.Copy(writer, r)
	if err != nil {
		return "", err
	}

	return "gs://" + object.BucketName() + "/" + object.ObjectName(), nil
}

func ocrPDF(ctx context.Context, path string) error {
	client, err := vision.NewImageAnnotatorClient(ctx)
	if err != nil {
		return err
	}

	request := &visionpb.AsyncBatchAnnotateFilesRequest{
		Requests: []*visionpb.AsyncAnnotateFileRequest{
			{
				Features: []*visionpb.Feature{
					{
						Type: visionpb.Feature_DOCUMENT_TEXT_DETECTION,
					},
				},
				InputConfig: &visionpb.InputConfig{
					GcsSource: &visionpb.GcsSource{Uri: path},
					MimeType:  "application/pdf",
				},
				OutputConfig: &visionpb.OutputConfig{
					GcsDestination: &visionpb.GcsDestination{
						Uri: path,
					},
				},
			},
		},
	}

	operation, err := client.AsyncBatchAnnotateFiles(ctx, request)
	if err != nil {
		return err
	}

	resp, err := operation.Wait(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("%v", resp)

	return nil
}

func waitingMessage(message string) func() {
	done := make(chan struct{})
	go func() {
		s := spin.New()
		for {
			select {
			case <-done:
				fmt.Printf("[done]\n\n")
				return
			default:
				fmt.Printf("\r%s %s", s.Next(), message)
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()

	return func() {
		close(done)
	}
}