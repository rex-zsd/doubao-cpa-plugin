package doubao

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func mediaFixtures(dir string) ([]byte, object, []byte, object, error) {
	random := token()
	red, blue := 3+int(random[0])%4, 2+int(random[1])%4
	picture := image.NewRGBA(image.Rect(0, 0, 720, 480))
	draw.Draw(picture, picture.Bounds(), &image.Uniform{color.White}, image.Point{}, draw.Src)
	for i := 0; i < red; i++ {
		x, y := 45+(i%3)*100, 60+(i/3)*150
		draw.Draw(picture, image.Rect(x, y, x+48, y+48), &image.Uniform{color.RGBA{225, 25, 35, 255}}, image.Point{}, draw.Src)
	}
	for i := 0; i < blue; i++ {
		x, y := 440+(i%2)*115, 80+(i/2)*125
		for dx := -26; dx <= 26; dx++ {
			for dy := -26; dy <= 26; dy++ {
				if dx*dx+dy*dy <= 26*26 {
					picture.SetRGBA(x+dx, y+dy, color.RGBA{20, 70, 230, 255})
				}
			}
		}
	}
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, picture); err != nil {
		return nil, nil, nil, nil, err
	}
	code := "FILE_NONCE_" + token()[:16]
	a, _ := strconv.ParseInt(random[2:6], 16, 32)
	b, _ := strconv.ParseInt(random[6:10], 16, 32)
	a, b = a%7000+1000, b%6000+2000
	stream := fmt.Sprintf("BT /F1 16 Tf 60 750 Td (Independent file ingestion probe) Tj 0 -35 Td (Verification code: %s) Tj 0 -35 Td (Amount A: %d) Tj 0 -35 Td (Amount B: %d) Tj ET", code, a, b)
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
	}
	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.4\n")
	offsets := []int{0}
	for i, obj := range objects {
		offsets = append(offsets, pdf.Len())
		fmt.Fprintf(&pdf, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := pdf.Len()
	fmt.Fprintf(&pdf, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&pdf, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&pdf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, nil, nil, nil, err
	}
	for name, data := range map[string][]byte{"fixture.png": pngData.Bytes(), "fixture.pdf": pdf.Bytes()} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			return nil, nil, nil, nil, err
		}
	}
	return pngData.Bytes(), object{"red_squares": red, "blue_circles": blue}, pdf.Bytes(), object{"code": code, "sum": a + b}, nil
}

func (c *Client) probeMedia(ctx context.Context, model Model, private, public string, evidence object) error {
	pngData, pictureExpected, pdfData, fileExpected, err := mediaFixtures(filepath.Join(filepath.Dir(private), "fixtures"))
	if err != nil {
		return err
	}
	cases := []struct {
		name     string
		data     []byte
		isImage  bool
		prompt   string
		expected object
	}{
		{"fixture.png", pngData, true, "请实际查看附带图片，数出红色正方形和蓝色圆形分别有多少个。只输出 JSON，键为 red_squares 和 blue_circles，值为整数，不要解释。", pictureExpected},
		{"fixture.pdf", pdfData, false, "请实际读取附带 PDF。提取 Verification code，并计算 Amount A 与 Amount B 的和。只输出 JSON，键为 code 和 sum，不要解释。", fileExpected},
	}
	rows := []any{}
	evidence["media"] = rows
	for _, item := range cases {
		id := token()
		row := object{"name": item.name, "image": item.isImage, "generation_posts": 0, "expected": item.expected, "nonce_absent_from_prompt": !strings.Contains(item.prompt, textValue(fileExpected["code"]))}
		rows = append(rows, row)
		evidence["media"] = rows
		record, uploadEvidence, err := c.uploadMedia(ctx, item.name, item.data, item.isImage, filepath.Join(private, "media-upload-"+id+".json"))
		row["upload"] = uploadEvidence
		if err != nil {
			row["error"] = err.Error()
			return err
		}
		body := buildBody(item.prompt, "", "work", model)
		appendMedia(body, []any{record})
		receiptPath := filepath.Join(private, "media-receipt-"+id+".json")
		row["generation_posts"] = 1
		receipt, err := c.run(ctx, body, receiptPath)
		if receipt != nil {
			row["accepted"], row["server_completed"], row["reply"] = receipt.Accepted, receipt.Completed, receipt.Reply
		}
		if err != nil {
			if receipt != nil && receipt.Accepted && !receipt.Completed {
				stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				row["cancellation_confirmed"] = c.stopProbe(stopCtx, receiptPath, object{}) == nil
				cancel()
			}
			row["error"] = err.Error()
			return err
		}
		answer := strings.TrimSpace(receipt.Reply)
		// Formatting fences are not part of semantic JSON; retain the raw reply.
		if strings.HasPrefix(answer, "```") {
			if n := strings.IndexByte(answer, '\n'); n >= 0 {
				answer = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(answer[n+1:]), "```"))
			}
		}
		row["exact_content_match"] = jsonText(parsed(answer)) == jsonText(item.expected)
		if row["exact_content_match"] != true {
			return errors.New("media contents did not match hidden fixture expectations")
		}
		if err = writeEvidence(filepath.Join(public, "partial.json"), evidence); err != nil {
			return err
		}
	}
	evidence["success"] = true
	return nil
}
