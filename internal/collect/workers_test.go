package collect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestParallelSync(t *testing.T) {
	for _, workers := range []int{1, 4} {
		t.Run(map[int]string{1: "serial", 4: "parallel"}[workers], func(t *testing.T) {
			db, c, f, started := setup(t, 251)
			var active, peak atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := active.Add(1)
				defer active.Add(-1)
				for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
				}
				time.Sleep(30 * time.Millisecond)
				f.ServeHTTP(w, r)
			}))
			defer server.Close()
			f.url = server.URL
			c.APIURL, c.GraphQLURL, c.Workers = server.URL, server.URL+"/graphql", workers
			begin := time.Now()
			result, err := syncAt(context.Background(), db, c, Options{}, started)
			if err != nil {
				t.Fatal(err)
			}
			if peak.Load() != int32(workers) || result.Requests != 20 || result.Status.Issues != 251 || result.Status.Comments != 1 {
				t.Fatal("parallel sync changed coverage or request count", peak.Load(), result)
			}
			t.Logf("%d workers: %s, %d requests, peak concurrency %d", workers, time.Since(begin).Round(time.Millisecond), result.Requests, peak.Load())
		})
	}
}
