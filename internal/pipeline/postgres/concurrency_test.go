package postgres_test

import (
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

func Test_concurrent_registration(t *testing.T) {
	t.Run("simultaneous identical creates converge", func(t *testing.T) {
		e := newEnv(t)
		src := pipelinetest.Doc(pipelinetest.Container("a", "args: [same]"))
		const n = 16
		results := make([]pipeline.RegisterResult, n)
		errs := make([]error, n)
		var start, done sync.WaitGroup
		start.Add(1)
		for i := 0; i < n; i++ {
			done.Add(1)
			go func(i int) {
				defer done.Done()
				start.Wait()
				results[i], errs[i] = e.register(scopeA, engineer, src)
			}(i)
		}
		start.Done()
		done.Wait()

		created := 0
		for i := range results {
			if errs[i] != nil {
				t.Fatalf("registration %d: %v", i, errs[i])
			}
			if results[i].Created {
				created++
			}
			if results[i].Definition.ID != results[0].Definition.ID || results[i].Definition.Version != 1 || results[i].Definition.Digest != results[0].Definition.Digest {
				t.Fatalf("registration %d diverged: %+v vs %+v", i, results[i].Definition, results[0].Definition)
			}
		}
		if created != 1 {
			t.Fatalf("exactly one registration creates the version, got %d", created)
		}
		if e.definitions(scopeA) != 1 || e.audits(scopeA) != 1 {
			t.Fatalf("rows: %d definitions, %d audits", e.definitions(scopeA), e.audits(scopeA))
		}
	})

	t.Run("different content under one name receives distinct ordered versions", func(t *testing.T) {
		e := newEnv(t)
		const n = 14
		results := make([]pipeline.RegisterResult, n)
		errs := make([]error, n)
		var start, done sync.WaitGroup
		start.Add(1)
		for i := 0; i < n; i++ {
			done.Add(1)
			go func(i int) {
				defer done.Done()
				start.Wait()
				results[i], errs[i] = e.register(scopeA, engineer, pipelinetest.Doc(pipelinetest.Container("a", fmt.Sprintf("args: [variant-%d]", i))))
			}(i)
		}
		start.Done()
		done.Wait()

		var versions []int
		digests := map[pipeline.Digest]bool{}
		ids := map[domain.DefinitionID]bool{}
		for i := range results {
			if errs[i] != nil {
				t.Fatalf("registration %d: %v", i, errs[i])
			}
			if !results[i].Created {
				t.Fatalf("registration %d was not created", i)
			}
			versions = append(versions, int(results[i].Definition.Version))
			digests[results[i].Definition.Digest], ids[results[i].Definition.ID] = true, true
		}
		sort.Ints(versions)
		for i, v := range versions {
			if v != i+1 {
				t.Fatalf("versions are not contiguous: %v", versions)
			}
		}
		if len(digests) != n || len(ids) != n {
			t.Fatalf("history was overwritten or merged: %d digests, %d ids", len(digests), len(ids))
		}
		if e.definitions(scopeA) != n || e.audits(scopeA) != n {
			t.Fatalf("rows: %d definitions, %d audits", e.definitions(scopeA), e.audits(scopeA))
		}
		// Every stored row still verifies against its own digest.
		for i := range results {
			got, err := e.svc.Get(t.Context(), scopeA, pipeline.Caller{PrincipalID: engineer}, results[i].Definition.ID)
			if err != nil || got.Digest != results[i].Definition.Digest || got.Version != results[i].Definition.Version {
				t.Fatalf("version %d changed: %+v %v", results[i].Definition.Version, got, err)
			}
		}
	})

	t.Run("a mixed burst of duplicates and new content is consistent", func(t *testing.T) {
		e := newEnv(t)
		const distinct, copies = 5, 4
		type outcome struct {
			res pipeline.RegisterResult
			err error
			i   int
		}
		out := make(chan outcome, distinct*copies)
		var start, done sync.WaitGroup
		start.Add(1)
		for i := 0; i < distinct; i++ {
			for c := 0; c < copies; c++ {
				done.Add(1)
				go func(i int) {
					defer done.Done()
					start.Wait()
					r, err := e.register(scopeA, engineer, pipelinetest.Doc(pipelinetest.Container("a", fmt.Sprintf("args: [v%d]", i))))
					out <- outcome{r, err, i}
				}(i)
			}
		}
		start.Done()
		done.Wait()
		close(out)
		idByVariant := map[int]domain.DefinitionID{}
		versions := map[domain.Version]bool{}
		for o := range out {
			if o.err != nil {
				t.Fatal(o.err)
			}
			if prev, ok := idByVariant[o.i]; ok && prev != o.res.Definition.ID {
				t.Fatalf("variant %d resolved to two definitions", o.i)
			}
			idByVariant[o.i] = o.res.Definition.ID
			versions[o.res.Definition.Version] = true
		}
		if len(idByVariant) != distinct || len(versions) != distinct || e.definitions(scopeA) != distinct || e.audits(scopeA) != distinct {
			t.Fatalf("variants=%d versions=%d definitions=%d audits=%d", len(idByVariant), len(versions), e.definitions(scopeA), e.audits(scopeA))
		}
	})

	t.Run("projects do not serialize or share versions", func(t *testing.T) {
		e := newEnv(t)
		src := pipelinetest.Doc(pipelinetest.Container("a"))
		a := e.mustRegister(scopeA, engineer, src)
		a2 := e.mustRegister(scopeA2, engineer, src)
		b := e.mustRegister(scopeB, engineer, src)
		if !a.Created || !a2.Created || !b.Created || a.Definition.Version != 1 || a2.Definition.Version != 1 || b.Definition.Version != 1 {
			t.Fatalf("each project starts its own history: %+v %+v %+v", a, a2, b)
		}
		if a.Definition.ID == a2.Definition.ID || a.Definition.ID == b.Definition.ID || a.Definition.Digest != a2.Definition.Digest {
			t.Fatal("identical content in other scopes is a separate definition with the same digest")
		}
	})
}
