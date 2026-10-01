package testutil

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"testing"
)

func MessageParts(t *testing.T, content []byte) (string, map[string]string) {
	t.Helper()
	message, err := mail.ReadMessage(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(message.Header.Get("Subject"))
	if err != nil {
		t.Fatal(err)
	}
	mediaType, params, err := mime.ParseMediaType(message.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/alternative" || params["boundary"] == "" {
		t.Fatal("邮件必须包含纯文本和 HTML 两种正文")
	}
	reader := multipart.NewReader(message.Body, params["boundary"])
	result := make(map[string]string)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		kind, attributes, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if err != nil || attributes["charset"] != "UTF-8" {
			t.Fatal("邮件正文编码不正确")
		}
		body, err := io.ReadAll(part)
		if err != nil {
			t.Fatal(err)
		}
		if _, duplicate := result[kind]; duplicate {
			t.Fatal("邮件正文类型重复")
		}
		result[kind] = string(body)
	}
	if len(result) != 2 || result["text/plain"] == "" || result["text/html"] == "" {
		t.Fatal("邮件缺少可读取的纯文本或 HTML 正文")
	}
	return subject, result
}
