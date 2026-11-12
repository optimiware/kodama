package pgtest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// unitTypeStates maps every commit-unit struct under internal/shell, by
// package-qualified name, to the observed unit×states that together prove it.
// Every listed state must be observed: a unit type is proved by all of its
// states, not by any one of them. A new *Unit type, a removed one or a
// renamed state breaks the gate until this table is updated on purpose.
var unitTypeStates = map[string][]string{
	"request.ActivationUnit":             {"activation.commit"},
	"request.ResolveUnit":                {"request.resolve", "request.resolve.fail", "request.resolve.pass.nonlast"},
	"task.CreateUnit":                    {"task.create", "task.create_and_start"},
	"task.EditUnit":                      {"task.edit.content", "task.edit.comment", "task.select_pipeline"},
	"task.StartUnit":                     {"task.start"},
	"task.AskUnit":                       {"task.ask"},
	"task.ReplyUnit":                     {"task.reply"},
	"task.CancelUnit":                    {"task.cancel"},
	"task.ProgressUnit":                  {"task.progress", "task.progress.stale", "task.progress.wrong_execution"},
	"execution.StartUnit":                {"execution.start"},
	"execution.TransitionUnit":           {"execution.transition"},
	"pipeline.ChangeUnit":                {"pipeline.apply", "pipeline.update", "pipeline.delete"},
	"trigger.SelectUnit":                 {"trigger.select"},
	"trigger.StartUnit":                  {"trigger.started"},
	"trigger.CompletionUnit":             {"trigger.complete"},
	"event.IngressUnit":                  {"ingress.publish", "ingress.replay"},
	"service.CreateUnit":                 {"identity.service.create"},
	"service.UpdateUnit":                 {"identity.service.update"},
	"service.StatusUnit":                 {"identity.service.enable", "identity.service.disable"},
	"token.MutationUnit":                 {"identity.credential.issue", "identity.credential.rotate", "identity.credential.revoke"},
	"resource.BindingUnit":               {"identity.binding.register", "identity.binding.update"},
	"resource.BindingDeleteUnit":         {"identity.binding.delete"},
	"tenancyadmin.TenantCreateUnit":      {"tenancy.tenant.create"},
	"tenancyadmin.TenantKeyRotationUnit": {"tenancy.tenant.key.rotate"},
	"tenancyadmin.ProjectCreateUnit":     {"tenancy.project.create"},
	"tenancyadmin.ProjectUpdateUnit":     {"tenancy.project.update"},
	"tenancyadmin.MembershipGrantUnit":   {"membership.first-grant.tenant-wide", "membership.first-grant.project", "tenancy.membership.grant.existing"},
	"tenancyadmin.MembershipRevokeUnit":  {"tenancy.membership.revoke"},
	"verify.RecordUnit":                  {"verification.record", "verification.record.with-intent"},
	"verify.ReportUnit":                  {"verification.report"},
}

// exemptUnitTypes lists *Unit types the component suite does not observe, with
// the concrete reason. The reason is mandatory.
var exemptUnitTypes = map[string]string{
	"store.Unit": "the generic store executor envelope; the observers record the concrete commit units each capability passes to it",
}

// nonUnitStates lists observed unit×states that no *Unit struct drives: they
// commit through the engine, the execution store, the gateway recorders, the
// outbox, the secret manager or a raw competing session. Each needs a reason.
// A state ending in .replay or .loser is accepted without an entry when its
// base state is mapped here or in unitTypeStates.
var nonUnitStates = map[string]string{
	"engine.commit":                                  "flow.Commit through the Engine port, not a capability unit",
	"engine.commit.cleanup-intent":                   "flow.Commit carrying a cleanup intent through the Engine port",
	"execution.cancel":                               "execution cancel commit through the execution store",
	"execution.outputs":                              "execution output submission commit through the execution store",
	"execution.settlement.failed":                    "settlement commit through the execution store",
	"execution.settlement.pass":                      "settlement commit through the execution store",
	"gateway.content.record":                         "gateway content recorder store unit",
	"gateway.usage.record":                           "gateway usage recorder store unit",
	"outbox.mark_published":                          "event outbox acknowledgement, not a capability unit",
	"secret.delete":                                  "secret manager commit",
	"secret.write":                                   "secret manager commit",
	"membership.first-grant.read-committed-commit":   "READ COMMITTED gap proof against a raw competing session",
	"membership.first-grant.read-committed-rollback": "READ COMMITTED gap proof against a raw competing session",
	"membership.grant.read-committed-version":        "READ COMMITTED row-lock proof against a raw competing session",
	"membership.first-grant.guard-only":              "absence guard proof with the unique index dropped on a throwaway shard",
	"identity.binding.register.conflict":             "resource register conflict race between two scopes",
}

// UnitStateCoverageError reports every gap between the *Unit types parsed from
// product source and the unit×states the suite actually observed, and refuses
// an empty registry.
func UnitStateCoverageError(root string) error {
	return unitStateCoverageError(root, DerivedUnitStates())
}

func unitStateCoverageError(root string, states map[string][]string) error {
	if len(states) == 0 {
		return fmt.Errorf("unit×state coverage: no observed unit×state is registered")
	}
	types, err := unitTypes(filepath.Join(root, "internal", "shell"))
	if err != nil {
		return fmt.Errorf("unit×state coverage: %w", err)
	}
	return unitStateCoverageFor(types, states, unitTypeStates, exemptUnitTypes, nonUnitStates)
}

// unitStateCoverageFor is the gate without the filesystem: every source type
// must be mapped or exempt, every mapped state observed, every observed state
// accounted for, and no table entry stale.
func unitStateCoverageFor(types []string, observed map[string][]string,
	mapping map[string][]string, exempt, nonUnit map[string]string) error {
	var problems []string
	source := make(map[string]bool, len(types))
	for _, typeName := range types {
		source[typeName] = true
	}
	accounted := map[string]string{}
	claim := func(state, owner string) {
		if previous, taken := accounted[state]; taken {
			problems = append(problems, fmt.Sprintf("state %q is claimed by both %s and %s", state, previous, owner))
			return
		}
		accounted[state] = owner
	}
	for _, typeName := range types {
		if reason, ok := exempt[typeName]; ok {
			if strings.TrimSpace(reason) == "" {
				problems = append(problems, fmt.Sprintf("exempt unit type %s has no reason", typeName))
			}
			continue
		}
		required, ok := mapping[typeName]
		if !ok || len(required) == 0 {
			problems = append(problems, fmt.Sprintf("unit type %s has no mapped unit×state", typeName))
			continue
		}
		for _, state := range required {
			claim(state, typeName)
			if _, seen := observed[state]; !seen {
				problems = append(problems, fmt.Sprintf("unit type %s: state %q was never observed", typeName, state))
			}
		}
	}
	for typeName := range mapping {
		if !source[typeName] {
			problems = append(problems, fmt.Sprintf("mapped unit type %s does not exist in product source", typeName))
		}
		if _, both := exempt[typeName]; both {
			problems = append(problems, fmt.Sprintf("unit type %s is both mapped and exempt", typeName))
		}
	}
	for typeName := range exempt {
		if !source[typeName] {
			problems = append(problems, fmt.Sprintf("exempt unit type %s does not exist in product source", typeName))
		}
	}
	for state, reason := range nonUnit {
		if strings.TrimSpace(reason) == "" {
			problems = append(problems, fmt.Sprintf("non-unit state %q has no reason", state))
		}
		claim(state, "nonUnitStates")
		if _, seen := observed[state]; !seen {
			problems = append(problems, fmt.Sprintf("non-unit state %q was never observed", state))
		}
	}
	for state := range observed {
		if _, ok := accounted[state]; ok {
			continue
		}
		if base, ok := strings.CutSuffix(state, ".replay"); ok && baseAccounted(base, accounted) {
			continue
		}
		if base, ok := strings.CutSuffix(state, ".loser"); ok && baseAccounted(base, accounted) {
			continue
		}
		problems = append(problems, fmt.Sprintf("observed state %q is not mapped to a unit type or listed as a non-unit state", state))
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("unit×state coverage:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

func baseAccounted(base string, accounted map[string]string) bool {
	_, ok := accounted[base]
	return ok
}

// unitTypes parses every non-test Go file under root and returns the struct
// types whose name ends in Unit as package-qualified names ("task.EditUnit"),
// sorted. Qualifying by package keeps same-named types of different packages
// distinct.
func unitTypes(root string) ([]string, error) {
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range parsed.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok || !strings.HasSuffix(typeSpec.Name.Name, "Unit") {
					continue
				}
				if _, structType := typeSpec.Type.(*ast.StructType); !structType {
					continue
				}
				seen[parsed.Name.Name+"."+typeSpec.Name.Name] = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	types := make([]string, 0, len(seen))
	for name := range seen {
		types = append(types, name)
	}
	sort.Strings(types)
	return types, nil
}
