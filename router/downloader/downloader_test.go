package downloader

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testDownloadID = "b6f1c2e4-0000-4000-8000-000000000000"

// TestDownload_MarshalJSON pins the payload the Panel reads when it lists the
// remote downloads in progress for a server.
func TestDownload_MarshalJSON(t *testing.T) {
	dl := &Download{Identifier: testDownloadID}
	_, _ = dl.counter(200).Write(make([]byte, 50))

	out, err := json.Marshal([]*Download{dl})
	require.NoError(t, err)
	assert.JSONEq(t, `[{"Identifier":"`+testDownloadID+`","Progress":0.25}]`, string(out))
}

// TestDownload_MarshalJSONWhileDownloading replays the production overlap: the
// download goroutine bumps the progress on every chunk while an API request
// marshals the tracked downloads. go test -race must stay silent and nothing
// may block.
func TestDownload_MarshalJSONWhileDownloading(t *testing.T) {
	dl := &Download{Identifier: testDownloadID}
	counter := dl.counter(1 << 20)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			_, _ = counter.Write(make([]byte, 512))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			_, err := json.Marshal([]*Download{dl})
			assert.NoError(t, err)
		}
	}()
	wg.Wait()

	assert.InDelta(t, float64(1000*512)/float64(1<<20), dl.Progress(), 1e-9)
}
