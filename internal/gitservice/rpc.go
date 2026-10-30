package gitservice

import (
	"context"
	"io"
	"net/http"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

const maxRPCBody = 1 << 20

// RPCHandler serves the routes the control plane calls under
// gitv1.WorkerBasePath (repository.Hosts, contracts/git/README.md). Every call
// is authenticated by the injected ControlCallerVerifier before anything else
// happens, is decoded strictly and is idempotent on the repository ID (and the
// operation ID of a mutation). Responses and errors use the contract's shapes.
func (s *Service) RPCHandler() http.Handler {
	base := gitv1.WorkerBasePath + "/repositories/{id}"
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+base+"/initialize", rpcPost(s, s.cfg.Limits.OperationTimeout, s.initialize))
	mux.HandleFunc("POST "+base+"/refs/list", rpcPost(s, s.cfg.Limits.OperationTimeout, s.readRefs))
	mux.HandleFunc("POST "+base+"/refs/values", rpcPost(s, s.cfg.Limits.OperationTimeout, s.readRefValues))
	mux.HandleFunc("POST "+base+"/journal", rpcPost(s, s.cfg.Limits.OperationTimeout, s.readJournal))
	mux.HandleFunc("POST "+base+"/retained-refs/set", rpcPost(s, s.cfg.Limits.OperationTimeout, s.setRetainedRef))
	mux.HandleFunc("POST "+base+"/retained-refs/delete", rpcPost(s, s.cfg.Limits.OperationTimeout, s.deleteRetainedRef))
	mux.HandleFunc("POST "+base+"/branches/create", rpcPost(s, s.cfg.Limits.OperationTimeout, s.createBranch))
	mux.HandleFunc("POST "+base+"/branches/delete", rpcPost(s, s.cfg.Limits.OperationTimeout, s.deleteBranch))
	mux.HandleFunc("POST "+base+"/maintenance", rpcPost(s, s.cfg.Limits.MaintenanceTimeout, s.maintain))
	mux.HandleFunc("POST "+base+"/objects/is-ancestor", rpcPost(s, s.cfg.Limits.OperationTimeout, s.isAncestorRPC))
	mux.HandleFunc("POST "+base+"/objects/has-commit", rpcPost(s, s.cfg.Limits.OperationTimeout, s.hasCommitRPC))
	mux.HandleFunc("POST "+base+"/objects/commit-time", rpcPost(s, s.cfg.Limits.OperationTimeout, s.commitTimeRPC))
	mux.HandleFunc("POST "+base+"/files", rpcPost(s, s.cfg.Limits.OperationTimeout, s.readFilesRPC))
	mux.HandleFunc("POST "+base+"/bundles/create", s.bundleCreateHandler)
	mux.HandleFunc("POST "+base+"/bundles/replay", s.bundleReplayHandler)
	mux.HandleFunc("POST "+base+"/restore-fence", rpcPost(s, s.cfg.Limits.OperationTimeout, s.setRestoreFence))
	mux.HandleFunc("GET "+base+"/receives/{receiveId}", func(w http.ResponseWriter, r *http.Request) {
		if !s.authenticateControl(w, r) {
			return
		}
		resp, err := s.receiveLiveness(r.PathValue("id"), r.PathValue("receiveId"))
		if err != nil {
			s.writeError(w, err, "")
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})
	return mux
}

// authenticateControl verifies the caller; on failure it has answered.
func (s *Service) authenticateControl(w http.ResponseWriter, r *http.Request) bool {
	if err := s.cfg.Callers.VerifyControlCall(r.Context(), r); err != nil {
		code := domain.CodeInvalidCredential
		if ce, ok := asContract(err); ok && (ce.Code == domain.CodeForbidden || ce.Code == domain.CodeUnavailable) {
			code = ce.Code
		}
		writeContractError(w, contractError(code, "the caller was refused"), "")
		return false
	}
	return true
}

// rpcPost adapts a typed operation: authenticate, read a bounded body, decode
// strictly, run with a deadline, encode the answer.
func rpcPost[Req any, P interface {
	*Req
	Validate() error
}, Resp any](s *Service, timeout time.Duration, fn func(ctx context.Context, pathID string, req Req) (Resp, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authenticateControl(w, r) {
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRPCBody))
		if err != nil {
			writeContractError(w, contractError(domain.CodeInvalidRequest, "the request body is unreadable or too large"), "")
			return
		}
		req, err := gitv1.DecodeRequest[Req, P](data)
		if err != nil {
			s.writeError(w, err, "")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		resp, err := fn(ctx, r.PathValue("id"), req)
		if err != nil {
			s.writeError(w, err, "")
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// Handler serves everything the worker exposes on one listener: the smart-HTTP
// Git endpoints, the control-plane RPC under gitv1.WorkerBasePath and a
// readiness endpoint. Anything else is 404.
func (s *Service) Handler() http.Handler {
	git, rpc := s.GitHandler(), s.RPCHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			w.WriteHeader(http.StatusOK)
		case len(r.URL.Path) > len(gitv1.WorkerBasePath) && r.URL.Path[:len(gitv1.WorkerBasePath)+1] == gitv1.WorkerBasePath+"/":
			rpc.ServeHTTP(w, r)
		default:
			git.ServeHTTP(w, r)
		}
	})
}
