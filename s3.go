package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// Store is an S3-compatible bucket (R2, AWS S3, Backblaze, MinIO...) where the site keeps the proxies.
// The site never moves video itself: it signs short-lived links, the Macs upload to them and the
// browsers play from them.
type Store struct {
	Endpoint string // https://ACCOUNT.r2.cloudflarestorage.com, https://s3.eu-central-1.amazonaws.com...
	Region   string // "auto" for R2
	Bucket   string
	Prefix   string // keys start with it, like "rushbin/"
	KeyID    string
	Secret   string
}

// storeFromEnv reads the store from env + ENDPOINT, _REGION, _BUCKET, _PREFIX, _KEY_ID, _SECRET (env is
// "RUSHBIN_S3_" for the proxies). Nil when not set: the site then works without proxies.
func storeFromEnv(env string) *Store {
	s := &Store{Endpoint: strings.TrimRight(os.Getenv(env+"ENDPOINT"), "/"), Region: os.Getenv(env + "REGION"),
		Bucket: os.Getenv(env + "BUCKET"), Prefix: os.Getenv(env + "PREFIX"),
		KeyID: os.Getenv(env + "KEY_ID"), Secret: os.Getenv(env + "SECRET")}
	if s.Endpoint == "" || s.Bucket == "" || s.KeyID == "" || s.Secret == "" {
		return nil
	}
	if s.Region == "" {
		s.Region = "auto"
	}
	return s
}

// releaseStore is where the site reads the releases: RUSHBIN_RELEASES_S3_* (a bucket of their own, with a key that
// can only read it), or else the main store under releases/. Nil when neither is set.
func releaseStore(main *Store) *Store {
	if s := storeFromEnv("RUSHBIN_RELEASES_S3_"); s != nil || main == nil {
		return s
	}
	s := *main
	s.Prefix += "releases/"
	return &s
}

// presign returns a link that allows method on key until expires, with AWS Signature Version 4 in the
// query string (path-style: endpoint/bucket/key), the scheme every S3-compatible store accepts.
// extra query parameters (like response-content-disposition) are signed too, as S3 requires.
func (s *Store) presign(method, key string, expires time.Duration, now time.Time, extra ...[2]string) string {
	u, _ := url.Parse(s.Endpoint)
	path := "/" + s.Prefix + key
	if s.Bucket != "" { // path-style; "" when the bucket is already in the endpoint's host name
		path = "/" + s.Bucket + path
	}
	day, stamp := now.UTC().Format("20060102"), now.UTC().Format("20060102T150405Z")
	scope := day + "/" + s.Region + "/s3/aws4_request"
	q := map[string]string{
		"X-Amz-Algorithm":     "AWS4-HMAC-SHA256",
		"X-Amz-Credential":    s.KeyID + "/" + scope,
		"X-Amz-Date":          stamp,
		"X-Amz-Expires":       fmt.Sprint(int(expires.Seconds())),
		"X-Amz-SignedHeaders": "host",
	}
	for _, e := range extra {
		q[e[0]] = e[1]
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var query []string
	for _, k := range keys {
		query = append(query, awsEscape(k, true)+"="+awsEscape(q[k], true))
	}
	canonicalQuery := strings.Join(query, "&")
	canonical := strings.Join([]string{method, awsEscape(path, false), canonicalQuery, "host:" + u.Host + "\n", "host", "UNSIGNED-PAYLOAD"}, "\n")
	sig := sigV4(s.Secret, scope, stamp, canonical)
	return u.Scheme + "://" + u.Host + awsEscape(path, false) + "?" + canonicalQuery + "&X-Amz-Signature=" + sig
}

// sigV4 is the signature of a canonical request under scope (day/region/service/aws4_request): its key is the
// secret folded through each part of the scope in turn.
func sigV4(secret, scope, stamp, canonical string) string {
	h := sha256.Sum256([]byte(canonical))
	k := []byte("AWS4" + secret)
	for _, part := range strings.Split(scope, "/") {
		k = hmacSHA256(k, part)
	}
	return hex.EncodeToString(hmacSHA256(k, "AWS4-HMAC-SHA256\n"+stamp+"\n"+scope+"\n"+hex.EncodeToString(h[:])))
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// awsEscape is the URI encoding of SigV4: unreserved characters stay, everything else is %XX in capitals;
// "/" stays in paths.
func awsEscape(s string, slash bool) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !slash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// do sends one request of the site's own to the store, signed for a minute; an answer other than 2xx is an error.
func (s *Store) do(ctx context.Context, method, key string, body []byte, extra ...[2]string) (*http.Response, error) {
	req, _ := http.NewRequestWithContext(ctx, method, s.presign(method, key, time.Minute, time.Now(), extra...), bytes.NewReader(body))
	res, err := client.Do(req)
	var ue *url.Error
	if errors.As(err, &ue) { // its message would carry the signed link: what failed, without it
		err = fmt.Errorf("%s %s: %w", method, key, ue.Err)
	}
	if err != nil {
		return nil, err
	}
	if res.StatusCode/100 != 2 {
		res.Body.Close()
		return nil, fmt.Errorf("%s %s: %s", method, key, res.Status)
	}
	return res, nil
}

// Multipart upload, for files over one PUT's 5 GiB: the site starts it and joins the parts, the Mac
// uploads each part to a link the site signs (CreateMultipartUpload, UploadPart, CompleteMultipartUpload,
// AbortMultipartUpload of the S3 API, the same on R2).

func (s *Store) createMultipart(ctx context.Context, key string) (string, error) {
	res, err := s.do(ctx, "POST", key, nil, [2]string{"uploads", ""})
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var out struct{ UploadId string }
	if err := xml.NewDecoder(io.LimitReader(res.Body, 65536)).Decode(&out); err != nil || out.UploadId == "" {
		return "", fmt.Errorf("the store did not start the upload: %v", err)
	}
	return out.UploadId, nil
}

// completeMultipart joins the parts, given the ETag the store answered for each, in order.
func (s *Store) completeMultipart(ctx context.Context, key, id string, etags []string) error {
	type part struct {
		PartNumber int
		ETag       string
	}
	in := struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Part    []part
	}{}
	for i, e := range etags {
		in.Part = append(in.Part, part{i + 1, e})
	}
	body, _ := xml.Marshal(in)
	res, err := s.do(ctx, "POST", key, body, [2]string{"uploadId", id})
	if err != nil {
		return err
	}
	return res.Body.Close() // S3 can answer 200 with an error inside: the HEAD that follows catches it
}

func (s *Store) abortMultipart(ctx context.Context, key, id string) {
	if res, err := s.do(ctx, "DELETE", key, nil, [2]string{"uploadId", id}); err == nil {
		res.Body.Close()
	}
}
