/*
Copyright 2011 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"

	"camlistore.org/pkg/auth"
	"camlistore.org/pkg/blobref"
	"camlistore.org/pkg/schema"
	"camlistore.org/pkg/search"
)

// A Client provides access to a Camlistore server.
type Client struct {
	// server is the input from user, pre-discovery.
	// For example "http://foo.com" or "foo.com:1234".
	// It is the responsibility of initPrefix to parse
	// server and set prefix, including doing discovery
	// to figure out what the proper server-declared
	// prefix is.
	server string

	prefixOnce sync.Once
	prefixErr  error
	prefixv    string // URL prefix before "/camli/"

	discoOnce      sync.Once
	discoErr       error
	searchRoot     string      // Handler prefix, or "" if none
	downloadHelper string      // or "" if none
	storageGen     string      // storage generation, or "" if not reported
	syncHandlers   []*SyncInfo // "from" and "to" url prefix for each syncHandler

	authMode auth.AuthMode

	httpClient *http.Client
	haveCache  HaveCache

	pendStatMu sync.Mutex           // guards pendStat
	pendStat   map[string][]statReq // blobref -> reqs; for next batch(es)

	statsMutex sync.Mutex
	stats      Stats

	// via maps the access path from a share root to a desired target.
	// It is non-nil when in "sharing" mode, where the Client is fetching
	// a share.
	via map[string]string // target => via (target is referenced from via)

	log     *log.Logger // not nil
	reqGate chan bool
}

const maxParallelHTTP = 5

// New returns a new Camlistore Client.
// The provided server is either "host:port" (assumed http, not https) or a
// URL prefix, with or without a path.
// Errors are not returned until subsequent operations.
func New(server string) *Client {
	return &Client{
		server:     server,
		httpClient: http.DefaultClient,
		reqGate:    make(chan bool, maxParallelHTTP),
		haveCache:  noHaveCache{},
	}
}

func NewOrFail() *Client {
	c := New(serverOrDie())
	c.log = log.New(os.Stderr, "", log.Ldate|log.Ltime)
	err := c.SetupAuth()
	if err != nil {
		log.Fatal(err)
	}
	return c
}

var shareURLRx = regexp.MustCompile(`^(.+)/camli/(` + blobref.Pattern + ")")

func NewFromShareRoot(shareBlobURL string) (c *Client, target *blobref.BlobRef, err error) {
	var root string
	if m := shareURLRx.FindStringSubmatch(shareBlobURL); m == nil {
		return nil, nil, fmt.Errorf("Unkown URL base; doesn't contain /camli/")
	} else {
		c = New(m[1])
		c.discoOnce.Do(func() { /* nothing */
		})
		c.prefixOnce.Do(func() { /* nothing */
		})
		c.prefixv = m[1]
		c.authMode = auth.None{}
		c.via = make(map[string]string)
		root = m[2]
	}
	res, err := http.Get(shareBlobURL)
	if err != nil {
		return nil, nil, fmt.Errorf("Error fetching %s: %v", shareBlobURL, err)
	}
	defer res.Body.Close()
	blob, err := schema.BlobFromReader(blobref.Parse(root), res.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("Error parsing JSON from %s: %v", shareBlobURL, err)
	}
	if blob.ShareAuthType() != "haveref" {
		return nil, nil, fmt.Errorf("Unknown share authType of %q", blob.ShareAuthType())
	}
	target = blob.ShareTarget()
	if target == nil {
		return nil, nil, fmt.Errorf("No target.")
	}
	c.via[target.String()] = root
	return c, target, nil
}

// SetHTTPClient sets the Camlistore client's HTTP client.
// If nil, the default HTTP client is used.
func (c *Client) SetHTTPClient(client *http.Client) {
	if client == nil {
		client = http.DefaultClient
	}
	c.httpClient = client
}

// A HaveCache caches whether a remote blobserver has a blob.
type HaveCache interface {
	StatBlobCache(br *blobref.BlobRef) (size int64, ok bool)
	NoteBlobExists(br *blobref.BlobRef, size int64)
}

type noHaveCache struct{}

func (noHaveCache) StatBlobCache(*blobref.BlobRef) (int64, bool) { return 0, false }
func (noHaveCache) NoteBlobExists(*blobref.BlobRef, int64)       {}

func (c *Client) SetHaveCache(cache HaveCache) {
	if cache == nil {
		cache = noHaveCache{}
	}
	c.haveCache = cache
}

func (c *Client) SetLogger(logger *log.Logger) {
	if logger == nil {
		c.log = log.New(ioutil.Discard, "", 0)
	} else {
		c.log = logger
	}
}

func (c *Client) Stats() Stats {
	c.statsMutex.Lock()
	defer c.statsMutex.Unlock()
	return c.stats // copy
}

// ErrNoSearchRoot is returned by SearchRoot if the server doesn't support search.
var ErrNoSearchRoot = errors.New("client: server doesn't support search")

// ErrNoStorageGeneration is returned by StorageGeneration if the
// server doesn't report a storage generation value.
var ErrNoStorageGeneration = errors.New("client: server doesn't report a storage generation")

// ErrNoSync is returned by SyncHandlers if the server does not advertise syncs.
var ErrNoSync = errors.New("client: server has no sync handlers")

// BlobRoot returns the server's blobroot URL prefix.
// If the client was constructed with an explicit path,
// that path is used. Otherwise the server's
// default advertised blobRoot is used.
func (c *Client) BlobRoot() (string, error) {
	prefix, err := c.prefix()
	if err != nil {
		return "", err
	}
	return prefix + "/", nil
}

// SearchRoot returns the server's search handler.
// If the server isn't running an index and search handler, the error
// will be ErrNoSearchRoot.
func (c *Client) SearchRoot() (string, error) {
	c.condDiscovery()
	if c.discoErr != nil {
		return "", c.discoErr
	}
	if c.searchRoot == "" {
		return "", ErrNoSearchRoot
	}
	return c.searchRoot, nil
}

// StorageGeneration returns the server's unique ID for its storage
// generation, reset whenever storage is reset, moved, or partially
// lost.
//
// This is a value that can be used in client cache keys to add
// certainty that they're talking to the same instance as previously.
//
// If the server doesn't return such a value, the error will be
// ErrNoStorageGeneration.
func (c *Client) StorageGeneration() (string, error) {
	c.condDiscovery()
	if c.discoErr != nil {
		return "", c.discoErr
	}
	if c.storageGen == "" {
		return "", ErrNoStorageGeneration
	}
	return c.storageGen, nil
}

// SyncInfo holds the data that were acquired with a discovery
// and that are relevant to a syncHandler.
type SyncInfo struct {
	From string
	To   string
}

// SyncHandlers returns the server's sync handlers "from" and
// "to" prefix URLs.
// If the server isn't running any sync handler, the error
// will be ErrNoSync.
func (c *Client) SyncHandlers() ([]*SyncInfo, error) {
	c.condDiscovery()
	if c.discoErr != nil {
		return nil, c.discoErr
	}
	if c.syncHandlers == nil {
		return nil, ErrNoSync
	}
	return c.syncHandlers, nil
}

var _ search.IGetRecentPermanodes = (*Client)(nil)

// SearchRecent implements search.IGetRecentPermanodes against a remote server over HTTP.
func (c *Client) GetRecentPermanodes(req *search.RecentRequest) (*search.RecentResponse, error) {
	sr, err := c.SearchRoot()
	if err != nil {
		return nil, err
	}
	url := sr + req.URLSuffix()
	hreq := c.newRequest("GET", url)
	hres, err := c.doReqGated(hreq)
	if err != nil {
		return nil, err
	}
	defer hres.Body.Close()
	res := new(search.RecentResponse)
	if err := json.NewDecoder(hres.Body).Decode(res); err != nil {
		return nil, err
	}
	if err := res.Err(); err != nil {
		return nil, err
	}
	return res, nil
}

// SearchExistingFileSchema does a search query looking for an
// existing file with entire contents of wholeRef, then does a HEAD
// request to verify the file still exists on the server.  If so,
// it returns that file schema's blobref.
//
// May return (nil, nil) on ENOENT. A non-nil error is only returned
// if there were problems searching.
func (c *Client) SearchExistingFileSchema(wholeRef *blobref.BlobRef) (*blobref.BlobRef, error) {
	sr, err := c.SearchRoot()
	if err != nil {
		return nil, err
	}
	url := sr + "camli/search/files?wholedigest=" + wholeRef.String()
	req := c.newRequest("GET", url)
	res, err := c.doReqGated(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var buf bytes.Buffer
	body := io.TeeReader(io.LimitReader(res.Body, 1<<20), &buf)
	type justWriter struct {
		io.Writer
	}
	if res.StatusCode != 200 {
		io.Copy(justWriter{ioutil.Discard}, body) // golang.org/issue/4589
		return nil, fmt.Errorf("client: got status code %d from URL %s; body %s", res.StatusCode, url, buf.String())
	}
	var ress struct {
		Files []*blobref.BlobRef `json:"files"`
	}
	if err := json.NewDecoder(body).Decode(&ress); err != nil {
		io.Copy(justWriter{ioutil.Discard}, body) // golang.org/issue/4589
		return nil, fmt.Errorf("client: error parsing JSON from URL %s: %v; body=%s", url, err, buf.String())
	}
	if len(ress.Files) == 0 {
		return nil, nil
	}
	for _, f := range ress.Files {
		if c.FileHasContents(f, wholeRef) {
			return f, nil
		}
	}
	return nil, nil
}

// FileHasContents returns true iff f refers to a "file" or "bytes" schema blob,
// the server is configured with a "download helper", and the server responds
// that all chunks of 'f' are available and match the digest of wholeRef.
func (c *Client) FileHasContents(f, wholeRef *blobref.BlobRef) bool {
	c.condDiscovery()
	if c.discoErr != nil {
		return false
	}
	if c.downloadHelper == "" {
		return false
	}
	req := c.newRequest("HEAD", c.downloadHelper+f.String()+"/?verifycontents="+wholeRef.String())
	res, err := c.doReqGated(req)
	if err != nil {
		log.Printf("download helper HEAD error: %v", err)
		return false
	}
	defer res.Body.Close()
	return res.Header.Get("X-Camli-Contents") == wholeRef.String()
}

func (c *Client) prefix() (string, error) {
	c.prefixOnce.Do(func() { c.initPrefix() })
	if c.prefixErr != nil {
		return "", c.prefixErr
	}
	if c.discoErr != nil {
		return "", c.discoErr
	}
	return c.prefixv, nil
}

func (c *Client) discoRoot() string {
	s := c.server
	if !strings.HasPrefix(s, "http") {
		s = "http://" + s
	}
	return s
}

// initPrefix uses the user provided server URL to define the URL
// prefix to the blobserver root. If the server URL has a path
// component then it is directly used, otherwise the blobRoot
// from the discovery is used as the path.
func (c *Client) initPrefix() {
	root := c.discoRoot()
	u, err := url.Parse(root)
	if err != nil {
		c.prefixErr = err
		return
	}
	if len(u.Path) > 1 {
		c.prefixv = strings.TrimRight(root, "/")
		return
	}
	c.condDiscovery()
}

func (c *Client) condDiscovery() {
	c.discoOnce.Do(func() { c.doDiscovery() })
}

func (c *Client) doDiscovery() {
	root, err := url.Parse(c.discoRoot())
	if err != nil {
		c.discoErr = err
		return
	}

	// If the path is just "" or "/", do discovery against
	// the URL to see which path we should actually use.
	req, _ := http.NewRequest("GET", c.discoRoot(), nil)
	req.Header.Set("Accept", "text/x-camli-configuration")
	c.authMode.AddAuthHeader(req)
	res, err := c.doReqGated(req)
	if err != nil {
		c.discoErr = err
		return
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		c.discoErr = fmt.Errorf("Got status %q from blobserver URL %q during configuration discovery", res.Status, c.discoRoot())
		return
	}
	// TODO(bradfitz): little weird in retrospect that we request
	// text/x-camli-configuration and expect to get back
	// text/javascript.  Make them consistent.
	if ct := res.Header.Get("Content-Type"); ct != "text/javascript" {
		c.discoErr = fmt.Errorf("Blobserver returned unexpected type %q from discovery", ct)
		return
	}
	m := make(map[string]interface{})
	if err := json.NewDecoder(res.Body).Decode(&m); err != nil {
		c.discoErr = err
		return
	}
	searchRoot, ok := m["searchRoot"].(string)
	if ok {
		u, err := root.Parse(searchRoot)
		if err != nil {
			c.discoErr = fmt.Errorf("client: invalid searchRoot %q; failed to resolve", searchRoot)
			return
		}
		c.searchRoot = u.String()
	}

	downloadHelper, ok := m["downloadHelper"].(string)
	if ok {
		u, err := root.Parse(downloadHelper)
		if err != nil {
			c.discoErr = fmt.Errorf("client: invalid downloadHelper %q; failed to resolve", downloadHelper)
			return
		}
		c.downloadHelper = u.String()
	}

	c.storageGen, _ = m["storageGeneration"].(string)

	blobRoot, ok := m["blobRoot"].(string)
	if !ok {
		c.discoErr = fmt.Errorf("No blobRoot in config discovery response")
		return
	}
	u, err := root.Parse(blobRoot)
	if err != nil {
		c.discoErr = fmt.Errorf("client: error resolving blobRoot: %v", err)
		return
	}
	c.prefixv = strings.TrimRight(u.String(), "/")

	syncHandlers, ok := m["syncHandlers"].([]interface{})
	if ok {
		for _, v := range syncHandlers {
			vmap := v.(map[string]interface{})
			from := vmap["from"].(string)
			ufrom, err := root.Parse(from)
			if err != nil {
				c.discoErr = fmt.Errorf("client: invalid %q \"from\" sync; failed to resolve", from)
				return
			}
			to := vmap["to"].(string)
			uto, err := root.Parse(to)
			if err != nil {
				c.discoErr = fmt.Errorf("client: invalid %q \"to\" sync; failed to resolve", to)
				return
			}
			c.syncHandlers = append(c.syncHandlers,
				&SyncInfo{From: ufrom.String(), To: uto.String()})
		}
	}
}

func (c *Client) newRequest(method, url string, body ...io.Reader) *http.Request {
	var bodyR io.Reader
	if len(body) > 0 {
		bodyR = body[0]
	}
	if len(body) > 1 {
		panic("too many body arguments")
	}
	req, err := http.NewRequest(method, url, bodyR)
	if err != nil {
		panic(err.Error())
	}
	// not done by http.NewRequest in Go 1.0:
	if br, ok := bodyR.(*bytes.Reader); ok {
		req.ContentLength = int64(br.Len())
	}
	c.authMode.AddAuthHeader(req)
	return req
}

func (c *Client) requestHTTPToken() {
	c.reqGate <- true
}

func (c *Client) releaseHTTPToken() {
	<-c.reqGate
}

func (c *Client) doReqGated(req *http.Request) (*http.Response, error) {
	c.requestHTTPToken()
	defer c.releaseHTTPToken()
	return c.httpClient.Do(req)
}
