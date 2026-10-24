package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The run handler writes its answer and then keeps working (retention against
// Git): the answer must be complete for the client while the handler runs on,
// and the connection must not be reused behind that work.
func TestCreatedRunAnswerIsReleasedBeforeTheHandlerReturns(t *testing.T) {
	release := make(chan struct{})
	finished := make(chan struct{})
	h := releaseCreated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"run_1"}`))
		<-release // confirmation of the retained reference
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	got := make(chan *http.Response, 1)
	go func() {
		resp, err := http.Post(srv.URL+APIPrefix+"/tenants/t/projects/p/runs", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Error(err)
			close(got)
			return
		}
		got <- resp
	}()
	select {
	case resp := <-got:
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || string(body) != `{"id":"run_1"}` || resp.StatusCode != http.StatusCreated {
			t.Fatalf("%d %q %v", resp.StatusCode, body, err)
		}
		if !resp.Close && resp.Header.Get("Connection") != "close" {
			t.Fatal("the connection must not be reused behind the running handler")
		}
		select {
		case <-finished:
			t.Fatal("the handler had already returned: the test does not prove early release")
		default:
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the answer was held until the handler returned")
	}
	close(release)
	<-finished
}

func TestOnlyRunCreationIsReleasedEarly(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		early        bool
	}{
		{"POST", APIPrefix + "/tenants/t/projects/p/runs", true},
		{"GET", APIPrefix + "/tenants/t/projects/p/runs", false},
		{"POST", APIPrefix + "/tenants/t/projects/p/work-items", false},
		{"POST", "/runs", false},
	} {
		var wrapped bool
		h := releaseCreated(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, wrapped = w.(flushingWriter)
		}))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(tc.method, tc.path, nil))
		if wrapped != tc.early {
			t.Errorf("%s %s: wrapped=%v", tc.method, tc.path, wrapped)
		}
	}
}
