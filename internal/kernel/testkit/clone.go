package testkit

import (
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"maps"
)

// The clone helpers keep the fake state isolated from its callers. Seed, Load
// and Commit all copy in and out, so a test that mutates a returned value
// cannot reach persisted state and a caller that mutates a committed value
// cannot rewrite history.

func cloneExecution(in kernel.Execution) kernel.Execution {
	out := in
	out.Snapshot = cloneSnapshot(in.Snapshot)
	if in.Actions != nil {
		out.Actions = make([]kernel.Action, len(in.Actions))
		for index, action := range in.Actions {
			out.Actions[index] = cloneAction(action)
		}
	}
	out.Failure = cloneFailure(in.Failure)
	return out
}

func cloneAction(in kernel.Action) kernel.Action {
	out := in
	out.Outputs = cloneOutputs(in.Outputs)
	out.StagedOutputs = cloneOutputs(in.StagedOutputs)
	out.Container = cloneContainerAction(in.Container)
	out.Request = cloneRequestAction(in.Request)
	return out
}

func cloneContainerAction(in *kernel.ContainerAction) *kernel.ContainerAction {
	if in == nil {
		return nil
	}
	out := *in
	out.Definition = cloneDefinitionContainer(in.Definition)
	out.Launch = in.Launch
	out.Observation = cloneObservation(in.Observation)
	return &out
}

func cloneDefinitionContainer(in definition.Container) definition.Container {
	out := in
	out.Commands = append([]string(nil), in.Commands...)
	out.Env = append([]definition.EnvVar(nil), in.Env...)
	out.Secrets = append([]id.SecretName(nil), in.Secrets...)
	out.Outputs = append([]id.OutputKey(nil), in.Outputs...)
	return out
}

func cloneRequestAction(in *kernel.RequestAction) *kernel.RequestAction {
	if in == nil {
		return nil
	}
	out := *in
	out.Definition = cloneDefinitionRequest(in.Definition)
	out.Subject = cloneSubjectPointer(in.Subject)
	out.Answer = cloneAnswer(in.Answer)
	return &out
}

func cloneDefinitionRequest(in definition.Request) definition.Request {
	out := in
	out.Subject = cloneDefinitionSubject(in.Subject)
	out.Outputs = append([]id.OutputKey(nil), in.Outputs...)
	return out
}

func cloneDefinitionSubject(in definition.Subject) definition.Subject {
	return definition.NewSubject(in.Fields())
}

func cloneSubjectPointer(in *definition.Subject) *definition.Subject {
	if in == nil {
		return nil
	}
	subject := cloneDefinitionSubject(*in)
	return &subject
}

func cloneOutputs(in kernel.Outputs) kernel.Outputs {
	if in == nil {
		return nil
	}
	out := make(kernel.Outputs, len(in))
	maps.Copy(out, in)
	return out
}

func cloneObservation(in kernel.Observation) kernel.Observation {
	out := in
	if in.ExitCode != nil {
		code := *in.ExitCode
		out.ExitCode = &code
	}
	if in.Stop != nil {
		stop := *in.Stop
		out.Stop = &stop
	}
	return out
}

func cloneAnswer(in *kernel.Answer) *kernel.Answer {
	if in == nil {
		return nil
	}
	out := *in
	out.Outputs = cloneOutputs(in.Outputs)
	out.Provenance = append([]byte(nil), in.Provenance...)
	return &out
}

func cloneFailure(in *kernel.Failure) *kernel.Failure {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneSnapshot(in *definition.Snapshot) *definition.Snapshot {
	if in == nil {
		return nil
	}
	task, _ := in.Task()
	return definition.NewSnapshot(definition.SnapshotInput{
		Pipeline:   in.Pipeline(),
		YAML:       in.YAML(),
		YAMLDigest: in.YAMLDigest(),
		Inputs:     in.Inputs(),
		Runtime:    in.Runtime(),
		Task:       task,
		TaskID:     in.TaskID(),
	})
}
