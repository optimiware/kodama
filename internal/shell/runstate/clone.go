package runstate

import (
	"maps"
	"slices"

	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// cloneRecord returns a deep copy of r that shares no slice, map or pointer
// with it. It copies values rather than round-tripping JSON, so times and every
// comparable field keep exactly the representation the engine supplied.
func cloneRecord(r runtime.RunRecord) runtime.RunRecord {
	r.Run = r.Run.Clone()
	r.Plan = clonePlan(r.Plan)
	r.Waits = cloneWaits(r.Waits)
	r.Commands = slices.Clone(r.Commands)
	r.Workloads = slices.Clone(r.Workloads)
	r.Resources = slices.Clone(r.Resources)
	return r
}

func cloneWaits(ws []runtime.Wait) []runtime.Wait {
	if ws == nil {
		return nil
	}
	out := make([]runtime.Wait, len(ws))
	for i, w := range ws {
		out[i] = cloneWait(w)
	}
	return out
}

func cloneWait(w runtime.Wait) runtime.Wait {
	w.Spec = cloneWaitSpec(w.Spec)
	w.Correlation = maps.Clone(w.Correlation)
	return w
}

func cloneWaitSpec(s runtime.WaitSpec) runtime.WaitSpec {
	s.Correlate = maps.Clone(s.Correlate)
	s.Outcomes = slices.Clone(s.Outcomes)
	return s
}

func cloneWorkload(w runtime.WorkloadSpec) runtime.WorkloadSpec {
	w.Commands = slices.Clone(w.Commands)
	w.Env = maps.Clone(w.Env)
	w.Secrets = slices.Clone(w.Secrets)
	w.Requires = slices.Clone(w.Requires)
	return w
}

func clonePlan(p runtime.Plan) runtime.Plan {
	if p.Steps != nil {
		steps := make([]runtime.PlanStep, len(p.Steps))
		for i, s := range p.Steps {
			s.DependsOn = slices.Clone(s.DependsOn)
			if s.Workload != nil {
				w := cloneWorkload(*s.Workload)
				s.Workload = &w
			}
			if s.Wait != nil {
				w := cloneWaitSpec(*s.Wait)
				s.Wait = &w
			}
			s.Requires = slices.Clone(s.Requires)
			s.Outputs = slices.Clone(s.Outputs)
			s.Bindings = maps.Clone(s.Bindings)
			steps[i] = s
		}
		p.Steps = steps
	}
	if p.Services != nil {
		services := make([]runtime.ServiceSpec, len(p.Services))
		for i, s := range p.Services {
			s.Commands = slices.Clone(s.Commands)
			s.Env = maps.Clone(s.Env)
			s.Secrets = slices.Clone(s.Secrets)
			s.Ports = slices.Clone(s.Ports)
			services[i] = s
		}
		p.Services = services
	}
	p.Inputs = maps.Clone(p.Inputs)
	return p
}

func cloneLookup(l runtime.SignalLookup) runtime.SignalLookup {
	if l.Record != nil {
		r := *l.Record
		l.Record = &r
	}
	if l.Tombstone != nil {
		t := *l.Tombstone
		l.Tombstone = &t
	}
	return l
}
