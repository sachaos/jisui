package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	vision "cloud.google.com/go/vision/apiv1"
	"github.com/golang/protobuf/jsonpb"
	"github.com/phpdave11/gofpdf"
	"github.com/phpdave11/gofpdf/contrib/gofpdi"
	"github.com/tj/go-spin"
	"google.golang.org/api/iterator"
	visionpb "google.golang.org/genproto/googleapis/cloud/vision/v1"
)

var (
	bucket string
	font   string
	output string
)

func main() {
	err := run()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		flag.Usage()
		os.Exit(1)
	}
}

func run() error {
	flag.StringVar(&bucket, "bucket", "", "GCS bucket")
	flag.StringVar(&font, "font", "", "font file (TTF)")
	flag.StringVar(&output, "output", "result.pdf", "output file name")
	flag.Parse()
	filename := flag.Arg(0)

	if filename == "" {
		return fmt.Errorf("invalid argument: filename must specified")
	}

	if bucket == "" {
		return fmt.Errorf("invalid argument: please specify -bucket flag")
	}

	if font == "" {
		return fmt.Errorf("invalid argument: please specify -font flag")
	}

	if output == "" {
		return fmt.Errorf("invalid argument: please specify -output flag")
	}

	ctx := context.Background()
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	endOfGCS := waitingMessage("waiting for GCS")
	bucket, path, err := uploadPDF(ctx, file)
	if err != nil {
		return err
	}
	endOfGCS()

	endOfOCR := waitingMessage("waiting for OCR")
	err = ocrPDF(ctx, bucket, path, bucket, path+"/output")
	if err != nil {
		return err
	}
	endOfOCR()

	endOfDL := waitingMessage("waiting for Downloading output")
	responses, err := downloadResponse(ctx, bucket, path+"/output")
	if err != nil {
		return err
	}
	endOfDL()

	file2, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file2.Close()

	endOfConvert := waitingMessage("annotating")
	err = integrateWithPDF(file2, collectAnnotations(responses), output)
	if err != nil {
		return err
	}
	endOfConvert()

	return nil
}

func collectAnnotations(responses []*visionpb.AnnotateFileResponse) map[int]*visionpb.TextAnnotation {
	annotations := map[int]*visionpb.TextAnnotation{}
	for _, response := range responses {
		for _, res := range response.Responses {
			annotations[int(res.Context.PageNumber)] = res.FullTextAnnotation
		}
	}
	return annotations
}

func uploadPDF(ctx context.Context, r io.Reader) (string, string, error) {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return "", "", err
	}

	name := strconv.FormatInt(time.Now().UnixNano(), 10)

	object := client.Bucket(bucket).Object(name)
	writer := object.NewWriter(ctx)
	defer writer.Close()

	_, err = io.Copy(writer, r)
	if err != nil {
		return "", "", err
	}

	return object.BucketName(), object.ObjectName(), nil
}

func ocrPDF(ctx context.Context, srcBucket, srcPath, dstBucket, dstPath string) error {
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
					GcsSource: &visionpb.GcsSource{Uri: fmt.Sprintf("gs://%s/%s", srcBucket, srcPath)},
					MimeType:  "application/pdf",
				},
				OutputConfig: &visionpb.OutputConfig{
					GcsDestination: &visionpb.GcsDestination{
						Uri: fmt.Sprintf("gs://%s/%s", dstBucket, dstPath),
					},
				},
			},
		},
	}

	operation, err := client.AsyncBatchAnnotateFiles(ctx, request)
	if err != nil {
		return err
	}

	_, err = operation.Wait(ctx)
	if err != nil {
		return err
	}

	return nil
}

func downloadResponse(ctx context.Context, bucket, prefix string) ([]*visionpb.AnnotateFileResponse, error) {
	var responses []*visionpb.AnnotateFileResponse

	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, err
	}

	it := client.Bucket(bucket).Objects(ctx, &storage.Query{
		Prefix: prefix,
	})

	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}

		object := client.Bucket(bucket).Object(attrs.Name)
		reader, err := object.NewReader(ctx)
		if err != nil {
			return nil, err
		}
		defer reader.Close()

		var data visionpb.AnnotateFileResponse
		m := jsonpb.Unmarshaler{}
		err = m.Unmarshal(reader, &data)
		if err != nil {
			return nil, err
		}

		responses = append(responses, &data)
	}

	return responses, err
}

func waitingMessage(message string) func() {
	var msg string
	msg = message[:]
	done := make(chan struct{})
	go func() {
		s := spin.New()
		for {
			select {
			case <-done:
				fmt.Printf("\r  %s [done]\n", msg)
				return
			default:
				fmt.Printf("\r%s %s", s.Next(), msg)
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()

	return func() {
		close(done)
	}
}

func integrateWithPDF(pdfr io.ReadSeeker, annotation map[int]*visionpb.TextAnnotation, result string) error {
	pdf := gofpdf.New("P", "pt", "A4", "")

	imp := gofpdi.NewImporter()
	tmpl := imp.ImportPageFromStream(pdf, &pdfr, 1, "/MediaBox")
	sizes := imp.GetPageSizes()
	nrPages := len(imp.GetPageSizes())

	pdf.AddUTF8Font("font", "", font)

	for i := 1; i <= nrPages; i++ {
		w := sizes[i]["/MediaBox"]["w"]
		h := sizes[i]["/MediaBox"]["h"]
		anno := annotation[i]
		pdf.AddPageFormat("P", gofpdf.SizeType{
			Wd: w,
			Ht: h,
		})

		for _, page := range anno.Pages {
			for _, block := range page.Blocks {
				for _, paragraph := range block.Paragraphs {
					for _, word := range paragraph.Words {
						minX, minY, _, maxY := extract(word.BoundingBox.NormalizedVertices)

						pdf.SetFont("font", "", (maxY-minY)*h)
						pdf.Text(w*minX, h*minY+(maxY-minY)*h, collectWords(word))
					}
				}
			}
		}

		if i > 1 {
			tmpl = imp.ImportPageFromStream(pdf, &pdfr, i, "/MediaBox")
		}
		imp.UseImportedTemplate(pdf, tmpl, 0, 0, w, h)
	}

	err := pdf.OutputFileAndClose(result)
	if err != nil {
		return err
	}

	return nil
}

func extract(vs []*visionpb.NormalizedVertex) (float64, float64, float64, float64) {
	minX := vs[0].X
	minY := vs[0].Y
	maxX := float32(0)
	maxY := float32(0)

	for _, v := range vs {
		if v.X < minX {
			minX = v.X
		}

		if v.Y < minY {
			minY = v.Y
		}

		if v.X > maxX {
			maxX = v.X
		}

		if v.Y > maxY {
			maxY = v.Y
		}
	}

	return float64(minX), float64(minY), float64(maxX), float64(maxY)
}

func collectWords(word *visionpb.Word) string {
	b := strings.Builder{}
	for _, symbol := range word.Symbols {
		b.WriteString(symbol.Text)
	}

	return b.String()
}
