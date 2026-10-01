package notifications

import (
	"bytes"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/textproto"
	"time"
)

type Mail struct {
	ID      string
	To      string
	Periods []Period
}

func Message(mail Mail, sender string) []byte {
	data := notificationEmailData(mail)
	var htmlBody bytes.Buffer
	_ = notificationEmailHTML.Execute(&htmlBody, data)
	var body bytes.Buffer
	parts := multipart.NewWriter(&body)
	for _, part := range []struct {
		contentType string
		body        []byte
	}{
		{"text/plain", []byte(notificationEmailText(data))},
		{"text/html", htmlBody.Bytes()},
	} {
		writer, _ := parts.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {part.contentType + "; charset=UTF-8"},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		encoded := quotedprintable.NewWriter(writer)
		_, _ = encoded.Write(part.body)
		_ = encoded.Close()
	}
	_ = parts.Close()
	return fmt.Appendf(nil, "From: %s\r\nTo: %s\r\nSubject: %s\r\nMessage-ID: <%s@usage-statistics.local>\r\nDate: %s\r\nMIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=%q\r\n\r\n%s", sender, mail.To, mime.QEncoding.Encode("UTF-8", data.Subject), mail.ID, time.Now().Format(time.RFC1123Z), parts.Boundary(), body.Bytes())
}
