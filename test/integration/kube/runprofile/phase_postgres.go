package runprofile

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// phasePostgres runs the PostgreSQL image as a non-root, user-namespaced,
// read-only-root service with bounded storage, gates on its readiness probe
// and round-trips data between two independent client Pods.
func (q *Qualifier) phasePostgres(ctx context.Context) error {
	ns := q.nsMain
	pw := randomHex(12)
	q.c.K.Rec.AddSecret(pw)
	if err := q.c.ApplyAll(ctx, q.secret(ns, "pg-cred", map[string]string{"password": pw})); err != nil {
		return err
	}
	db := q.base("postgres", q.img("postgres"), 70)
	db.Labels = map[string]any{"krp/svc": "postgres"}
	db.FSGroup = 70
	db.ReadOnlyFS = true
	db.MemLimit = "1Gi"
	db.Env = []any{secretEnv("POSTGRES_PASSWORD", "pg-cred", "password"), plainEnv("POSTGRES_USER", "kodama"),
		plainEnv("POSTGRES_DB", "app"), plainEnv("PGDATA", "/data/pgdata")}
	db.Volumes = []any{emptyDir("data", "512Mi", false), emptyDir("run", "16Mi", false), emptyDir("tmp", "64Mi", false)}
	db.Mounts = []any{mount("data", "/data", false), mount("run", "/var/run/postgresql", false), mount("tmp", "/tmp", false)}
	db.Readiness = map[string]any{"exec": map[string]any{"command": []any{"pg_isready", "-U", "kodama", "-d", "app", "-h", "127.0.0.1"}}, "periodSeconds": 3, "timeoutSeconds": 3}
	db.Restart = "Never"
	if err := q.c.ApplyAll(ctx, q.c.PodObject(db), q.service(ns, "postgres", 5432)); err != nil {
		return err
	}
	if err := q.c.WaitReady(ctx, ns, db.Name, 8*minute); err != nil {
		q.add("service.postgres", "PostgreSQL service", true, Fail, "readiness never succeeded: "+err.Error(), grepLines(q.c.logs(ctx, ns, db.Name), "FATAL", "ERROR", "denied", "Permission"), "")
		return nil
	}
	if pj, err := q.c.GetPod(ctx, ns, db.Name); err == nil {
		q.noteImage(pj, db.Image)
	}
	host := "postgres." + ns + ".svc.cluster.local"
	client := func(name string) Pod {
		c := q.base(name, q.img("postgres"), 20002)
		c.Labels = map[string]any{"krp/role": "client"}
		c.Env = []any{secretEnv("PGPASSWORD", "pg-cred", "password"), plainEnv("PGHOST", host), plainEnv("PGUSER", "kodama"), plainEnv("PGDATABASE", "app"), plainEnv("PGCONNECT_TIMEOUT", "10")}
		return c
	}
	outA, codeA, errA := q.oneShot(ctx, ns, client("pg-client-a"), `psql -v ON_ERROR_STOP=1 -qAt -c "create table roundtrip(id int primary key, note text)" -c "insert into roundtrip values (1, 'run-profile')" -c "select 'wrote:' || count(*) from roundtrip"; psql -qAt -c "show data_directory" -c "select current_user, inet_server_addr() is not null"`, 4*minute)
	outB, codeB, errB := q.oneShot(ctx, ns, client("pg-client-b"), `psql -v ON_ERROR_STOP=1 -qAt -c "select 'read:' || note from roundtrip where id = 1"; PGPASSWORD=wrong psql -qAt -c "select 1" 2>&1 | head -1`, 4*minute)
	uid := strings.TrimSpace(q.execOrEmpty(ctx, ns, db.Name, `echo "server_uid=$(id -u) uid_map=$(tr -s ' ' < /proc/self/uid_map)"; grep -c . /data/pgdata/PG_VERSION`))
	status, detail := Pass, "non-root (uid 70, user-namespaced, read-only root) PostgreSQL became Ready, accepted a write from one client Pod, and a second client Pod read it; a wrong password was refused"
	switch {
	case errA != nil || errB != nil || codeA != 0 || codeB != 0:
		status, detail = Fail, fmt.Sprintf("client roundtrip failed: a(exit %d, %v) b(exit %d, %v)", codeA, errA, codeB, errB)
	case !strings.Contains(outB, "read:run-profile"):
		status, detail = Fail, "second client did not read the first client's row"
	case !strings.Contains(outB, "authentication failed"):
		status, detail = Fail, "wrong password was not refused"
	case !strings.Contains(uid, "server_uid=70"):
		status, detail = Fail, "server did not run as uid 70"
	}
	q.add("service.postgres", "PostgreSQL service", true, status, detail, append(lines(outA, 4), append(lines(outB, 3), uid)...), "")
	return nil
}

func (q *Qualifier) execOrEmpty(ctx context.Context, ns, pod, script string) string {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, _ := q.c.Exec(cctx, ns, pod, nil, script)
	return out
}
