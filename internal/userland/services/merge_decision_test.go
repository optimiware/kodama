package services_test

import (
	"context"
	"slices"
	"testing"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/userland/services"
)

func TestParseMergeSubjectSupportsOptionalDecisionID(t *testing.T) {
	fields := append(mergeSubjectFields("shop/web", "refs/heads/main", "base-111", "cand-222"),
		api.SubjectField{Name: services.MergeFieldDecisionID, Kind: "string", Value: "dec_abc123"},
	)
	parsed, err := services.ParseMergeSubject(mergeSubject(fields))
	if err != nil {
		t.Fatalf("ParseMergeSubject(with decision_id): %v", err)
	}
	if parsed.DecisionID != id.DecisionID("dec_abc123") {
		t.Fatalf("DecisionID = %q; want dec_abc123", parsed.DecisionID)
	}

	// Invalid decision_id shapes are refused.
	for _, tc := range []struct {
		name  string
		field api.SubjectField
	}{
		{"empty decision_id", api.SubjectField{Name: services.MergeFieldDecisionID, Kind: "string", Value: ""}},
		{"path traversal decision_id", api.SubjectField{Name: services.MergeFieldDecisionID, Kind: "string", Value: "../escape"}},
		{"non-string decision_id", api.SubjectField{Name: services.MergeFieldDecisionID, Kind: "number", Value: "123"}},
		{"unknown extra field", api.SubjectField{Name: "extra", Kind: "string", Value: "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := append(mergeSubjectFields("shop/web", "refs/heads/main", "base-111", "cand-222"), tc.field)
			if _, err := services.ParseMergeSubject(mergeSubject(bad)); err == nil {
				t.Fatalf("ParseMergeSubject accepted %s", tc.name)
			}
		})
	}
}

func TestMergeCoordinatorForwardsDecisionIDToGitPushOptions(t *testing.T) {
	var seenPushArgs []string
	exec := &decisionGitExecutor{
		run: func(cmd services.GitCommand) (services.GitCommandResult, error) {
			if len(cmd.Args) == 0 {
				return services.GitCommandResult{}, nil
			}
			switch cmd.Args[0] {
			case "init", "remote", "fetch":
				return services.GitCommandResult{ExitCode: 0}, nil
			case "rev-parse":
				arg := cmd.Args[len(cmd.Args)-1]
				if arg == "base-111^{commit}" {
					return services.GitCommandResult{Stdout: []byte("1111111111111111111111111111111111111111\n")}, nil
				}
				return services.GitCommandResult{Stdout: []byte("2222222222222222222222222222222222222222\n")}, nil
			case "ls-remote":
				if len(seenPushArgs) > 0 {
					return services.GitCommandResult{Stdout: []byte("2222222222222222222222222222222222222222\trefs/heads/main\n")}, nil
				}
				return services.GitCommandResult{Stdout: []byte("1111111111111111111111111111111111111111\trefs/heads/main\n")}, nil
			case "merge-base":
				if cmd.Args[2] == "1111111111111111111111111111111111111111" {
					return services.GitCommandResult{ExitCode: 0}, nil
				}
				return services.GitCommandResult{ExitCode: 1}, nil
			case "push":
				seenPushArgs = append([]string(nil), cmd.Args...)
				return services.GitCommandResult{ExitCode: 0}, nil
			default:
				return services.GitCommandResult{ExitCode: 0}, nil
			}
		},
	}

	effects, err := services.NewGitMergeEffects(services.GitMergeEffectsConfig{
		RemoteBaseURL:  "file:///tmp/remotes",
		StateDirectory: t.TempDir(),
		Executor:       exec,
	})
	if err != nil {
		t.Fatalf("NewGitMergeEffects: %v", err)
	}
	obs, err := effects.Execute(context.Background(), services.MergeOperation{
		Key:         services.MergeKey{Repository: "shop/web", Ref: "refs/heads/main"},
		OperationID: "op-merge-dec-1",
		Base:        "base-111",
		Candidate:   "cand-222",
		DecisionID:  id.DecisionID("dec_merge_forward_1"),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if obs.State != services.EffectApplied {
		t.Fatalf("observation state = %q; want applied", obs.State)
	}
	idx := slices.Index(seenPushArgs, "-o")
	if idx < 0 || idx+1 >= len(seenPushArgs) || seenPushArgs[idx+1] != "kodema.decision_id=dec_merge_forward_1" {
		t.Fatalf("git push args = %v; want -o kodema.decision_id=dec_merge_forward_1", seenPushArgs)
	}
}

type decisionGitExecutor struct {
	run func(services.GitCommand) (services.GitCommandResult, error)
}

func (s *decisionGitExecutor) Run(_ context.Context, cmd services.GitCommand) (services.GitCommandResult, error) {
	return s.run(cmd)
}
