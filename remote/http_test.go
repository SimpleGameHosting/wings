package remote

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// roundTripperFunc adapts a function into an HTTP transport for focused client
// lifecycle tests.
type roundTripperFunc func(*http.Request) (*http.Response, error)

// RoundTrip executes the adapted transport function.
func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

// closeTrackingBody records when the remote client releases a response body.
type closeTrackingBody struct {
	closed *bool
}

// Read provides an empty response payload.
func (b closeTrackingBody) Read([]byte) (int, error) {
	return 0, io.EOF
}

// Close records that the response can be reused by the HTTP transport.
func (b closeTrackingBody) Close() error {
	*b.closed = true

	return nil
}

func createTestClient(h http.HandlerFunc) (*client, *httptest.Server) {
	s := httptest.NewServer(h)
	c := &client{
		httpClient:  s.Client(),
		baseUrl:     s.URL,
		maxAttempts: 1,
		tokenId:     "testid",
		token:       "testtoken",
	}
	return c, s
}

func TestRequest(t *testing.T) {
	c, _ := createTestClient(func(rw http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "application/vnd.pterodactyl.v1+json", r.Header.Get("Accept"))
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		assert.Equal(t, "Bearer testid.testtoken", r.Header.Get("Authorization"))
		assert.Equal(t, "/test", r.URL.Path)

		rw.WriteHeader(http.StatusOK)
	})
	r, err := c.requestOnce(context.Background(), "", "/test", nil)
	assert.NoError(t, err)
	assert.NotNil(t, r)
}

func TestSetCredentials(t *testing.T) {
	var authorization []string
	c, server := createTestClient(func(rw http.ResponseWriter, r *http.Request) {
		authorization = append(authorization, r.Header.Get("Authorization"))
		rw.WriteHeader(http.StatusOK)
	})
	defer server.Close()

	if _, err := c.requestOnce(context.Background(), http.MethodGet, "/test", nil); err != nil {
		t.Fatal(err)
	}
	c.SetCredentials("rotated-id", "rotated-token")
	if _, err := c.requestOnce(context.Background(), http.MethodGet, "/test", nil); err != nil {
		t.Fatal(err)
	}

	assert.Equal(t, []string{
		"Bearer testid.testtoken",
		"Bearer rotated-id.rotated-token",
	}, authorization)
}

func TestRequestRetry(t *testing.T) {
	// Test if the client attempts failed requests
	i := 0
	c, _ := createTestClient(func(rw http.ResponseWriter, r *http.Request) {
		if i < 1 {
			rw.WriteHeader(http.StatusInternalServerError)
		} else {
			rw.WriteHeader(http.StatusOK)
		}
		i++
	})
	c.maxAttempts = 2
	r, err := c.request(context.Background(), "", "", nil)
	assert.NoError(t, err)
	assert.NotNil(t, r)
	assert.Equal(t, http.StatusOK, r.StatusCode)
	assert.Equal(t, 2, i)

	// Test whether the client returns the last request after retry limit is reached
	i = 0
	c, _ = createTestClient(func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(http.StatusInternalServerError)
		i++
	})
	c.maxAttempts = 2
	r, err = c.request(context.Background(), "get", "", nil)
	assert.Error(t, err)
	assert.Nil(t, r)

	v := AsRequestError(err)
	assert.NotNil(t, v)
	assert.Equal(t, http.StatusInternalServerError, v.StatusCode())
	assert.Equal(t, 3, i)
}

// TestRequestClosesFailedResponseBeforeRetry ensures a retry does not retain
// each failed connection body until the entire backoff loop finishes.
func TestRequestClosesFailedResponseBeforeRetry(t *testing.T) {
	firstBodyClosed := false
	closedBeforeSecondRequest := false
	attempts := 0
	c := &client{
		baseUrl:     "http://panel.test",
		maxAttempts: 1,
		httpClient: &http.Client{Transport: roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
			attempts++
			if attempts == 2 {
				closedBeforeSecondRequest = firstBodyClosed
			}

			bodyClosed := &firstBodyClosed
			if attempts == 2 {
				bodyClosed = new(bool)
			}

			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Header:     make(http.Header),
				Body:       closeTrackingBody{closed: bodyClosed},
			}, nil
		})},
	}

	_, err := c.request(context.Background(), http.MethodGet, "/test", nil)

	assert.Error(t, err)
	assert.Equal(t, 2, attempts)
	assert.True(t, closedBeforeSecondRequest)
}

func TestGet(t *testing.T) {
	c, _ := createTestClient(func(rw http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Len(t, r.URL.Query(), 1)
		assert.Equal(t, "world", r.URL.Query().Get("hello"))
	})
	r, err := c.Get(context.Background(), "/test", q{"hello": "world"})
	assert.NoError(t, err)
	assert.NotNil(t, r)
}

func TestPost(t *testing.T) {
	test := map[string]string{
		"hello": "world",
	}
	c, _ := createTestClient(func(rw http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
	})
	r, err := c.Post(context.Background(), "/test", test)
	assert.NoError(t, err)
	assert.NotNil(t, r)
}
