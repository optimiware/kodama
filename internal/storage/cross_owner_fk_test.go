package storage_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/optimiware/kodama/internal/storage"
)

func Test_cross_owner_fk(t *testing.T) {
	e := newEnv(t)
	e.exec(t, `
INSERT INTO repositories (tenant_id,project_id,id,name) VALUES
 ('tenant-a','pa1','ra','code'),('tenant-a','pa2','ra2','code'),('tenant-b','pb1','rb','code');
INSERT INTO pipelines (tenant_id,project_id,id,repo_id,name,definition,source) VALUES
 ('tenant-a','pa1','pl','ra','test','{"steps":[]}','api');
INSERT INTO tasks (tenant_id,project_id,id,title,state) VALUES
 ('tenant-a','pa1','task','Task','plan'),('tenant-a','pa2','other','Other','plan');
INSERT INTO runs (tenant_id,project_id,id,pipeline_id,definition_snapshot) VALUES
 ('tenant-a','pa1','run','pl','{"steps":[]}'),('tenant-a','pa1','other-run','pl','{"steps":[]}');
INSERT INTO run_steps (tenant_id,project_id,run_id,id,step_key,ordinal,image) VALUES
 ('tenant-a','pa1','run','step','test',0,'example.invalid/test@sha256:abc');
INSERT INTO secrets (tenant_id,project_id,id,name,encrypted_value,key_id,envelope) VALUES
 ('tenant-a',NULL,'shared','KEY','ciphertext','key','{}'),
 ('tenant-a','pa2','private','KEY','ciphertext','key','{}');`)
	cases := []struct{ name, sql, code string }{
		{"membership cross tenant", `INSERT INTO memberships (tenant_id,id,user_id,project_id,role) VALUES ('tenant-a','bad','u1','pb1','viewer')`, "23503"},
		{"pipeline cross project", `INSERT INTO pipelines (tenant_id,project_id,id,repo_id,name,definition,source) VALUES ('tenant-a','pa2','bad','ra','test','{}','api')`, "23503"},
		{"task cross project", `UPDATE tasks SET blocked_by_id='other', parked_at=now(), resume_state='plan' WHERE tenant_id='tenant-a' AND project_id='pa1'`, "23503"},
		{"task self block", `UPDATE tasks SET blocked_by_id=id, parked_at=now(), resume_state='plan' WHERE id='task'`, "23514"},
		{"incomplete parking", `UPDATE tasks SET parked_at=now() WHERE id='task'`, "23514"},
		{"run cross project", `INSERT INTO runs (tenant_id,project_id,id,pipeline_id,definition_snapshot) VALUES ('tenant-a','pa2','bad','pl','{}')`, "23503"},
		{"service tenant with project", `INSERT INTO services (tenant_id,project_id,id,scope,name,image) VALUES ('tenant-a','pa1','bad','tenant','db','postgres')`, "23514"},
		{"service pipeline without run", `INSERT INTO services (tenant_id,project_id,id,scope,name,image) VALUES ('tenant-a','pa1','bad','pipeline','db','postgres')`, "23514"},
		{"service cross project run", `INSERT INTO services (tenant_id,project_id,run_id,id,scope,name,image) VALUES ('tenant-a','pa2','run','bad','pipeline','db','postgres')`, "23503"},
		{"artifact wrong run step", `INSERT INTO artifacts (tenant_id,project_id,id,run_id,step_id,name,kind,uri) VALUES ('tenant-a','pa1','bad','other-run','step','preview','preview','https://example.invalid')`, "23503"},
		{"session wrong run step", `INSERT INTO agent_sessions (tenant_id,project_id,id,run_id,step_id,model,status) VALUES ('tenant-a','pa1','bad','other-run','step','model','running')`, "23503"},
		{"MCP other project credential", `INSERT INTO mcp_servers (tenant_id,project_id,id,name,url,secret_id,secret_project_id) VALUES ('tenant-a','pa1','bad','tools','https://example.invalid','private','pa2')`, "23514"},
		{"MCP private credential as shared", `INSERT INTO mcp_servers (tenant_id,project_id,id,name,url,secret_id) VALUES ('tenant-a','pa1','bad','tools','https://example.invalid','private')`, "23503"},
		{"duplicate tenant secret", `INSERT INTO secrets (tenant_id,id,name,encrypted_value,key_id,envelope) VALUES ('tenant-a','bad','KEY','ciphertext','key','{}')`, "23505"},
		{"definition object", `UPDATE pipelines SET definition='[]'`, "23514"},
		{"unknown role", `INSERT INTO memberships (tenant_id,id,user_id,role) VALUES ('tenant-a','bad','u1','superadmin')`, "23514"},
		{"duplicate tenant membership", `INSERT INTO memberships (tenant_id,id,user_id,role) VALUES ('tenant-a','bad','u1','admin')`, "23505"},
		{"duplicate issuer", `INSERT INTO users (id,issuer,subject) VALUES ('bad','https://issuer.example','subject-1')`, "23505"},
		{"project workflow object", `UPDATE projects SET task_workflow='[]'`, "23514"},
		{"positive version", `UPDATE projects SET version=0`, "23514"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := e.mig.Exec(testCtx(t), c.sql)
			requireCode(t, err, c.code, c.name)
		})
	}
	e.exec(t, `INSERT INTO mcp_servers (tenant_id,project_id,id,name,url,secret_id) VALUES
 ('tenant-a','pa1','shared-client','tools','https://example.invalid','shared');
INSERT INTO services (tenant_id,id,scope,name,image) VALUES ('tenant-a','tenant-db','tenant','db','postgres');
INSERT INTO services (tenant_id,project_id,id,scope,name,image) VALUES ('tenant-a','pa1','project-db','project','db','postgres');
INSERT INTO services (tenant_id,project_id,run_id,id,scope,name,image) VALUES ('tenant-a','pa1','run','run-db','pipeline','db','postgres');`)
	pool := e.runtime(t, 1)
	for _, sql := range []string{
		`INSERT INTO projects (tenant_id,id,key,task_workflow) VALUES ('tenant-b','bad','bad','{}')`,
		`INSERT INTO services (tenant_id,id,scope,name,image) VALUES ('tenant-b','bad','tenant','db','postgres')`,
		`INSERT INTO audit_events (tenant_id,id,actor_type,actor_id,event_type,resource_type,resource_id) VALUES ('tenant-b','bad','system','system','test','project','pb1')`,
	} {
		err := storage.WithTenant(testCtx(t), pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql)
			return err
		})
		requireCode(t, err, "42501", sql)
	}
}
