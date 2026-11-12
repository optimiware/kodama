"""The 51 single-edit mutant definitions of the PostgreSQL store mutant gate.

Executed by gate.py; each add(name, file, regex, replacement[, occurrence]) edits
one product or test-support source file in a scratch copy of the tree.
"""
import sys,shutil,os,subprocess,re
U="internal/shell/store/units/"
P="internal/shell/store/postgres/"
S="test/support/infrastructure/postgres/"
M={}
def add(n,f,a,b,k=1): M[n]=(f,a,b,k)
add("PRG_done",U+"work/task.go",r'string\(task\.StateDone\)','string(task.StateOpen)')
add("RES_task",U+"work/adapters.go",r'record\.Result = encoded','record.Result = append([]byte("{}"), encoded[:0]...)')
add("FAIL_json",U+"execution/codec.go",r'(execution failure could not be encoded", err\)\n\t\}\n\treturn )encoded(, nil)',r'\1append([]byte("{}"), encoded[:0]...)\2')
add("SNAP_exec",U+"execution/start.go",r'"snapshot":     snapshot,','"snapshot": append([]byte("{}"), snapshot[:0]...),')
add("SNAP_task",U+"work/task.go",r'"snapshot":         document,','"snapshot": append([]byte("{}"), document[:0]...),')
add("NONCE_zero",U+"identity/credential.go",r'return credential\.Nonce','return make([]byte, len(credential.Nonce))')
add("EV_type",P+"unit.go",r'project, string\(envelope\.Type\),','project, string(envelope.Type)+".x",')
add("EV_subject",P+"unit.go",r'reportType, envelope\.SubjectID, subjectVersion','reportType, envelope.SubjectID+"x", subjectVersion')
add("EV_data",P+"unit.go",r'data = envelope\.Data','data = []byte("{}")')
add("EV_src",P+"unit.go",r'string\(envelope\.Source\.PrincipalID\)','string(envelope.Source.PrincipalID)+"x"')
add("RC_caller",P+"unit.go",r'record\.Key\.Operation, record\.Key\.CallerKey, contentHash, result, completed','record.Key.Operation, record.Key.CallerKey+"x", contentHash, result, completed')
add("RC_op",P+"unit.go",r'record\.Key\.Operation, record\.Key\.CallerKey, contentHash, result, completed','record.Key.Operation+"x", record.Key.CallerKey, contentHash, result, completed')
add("RC_created",P+"unit.go",r'execution, snapshot, record\.At,','execution, snapshot, record.At.AddDate(0, 0, 1),')
add("AU_detail",P+"unit.go",r'var detail any\n','var detail any = ""\n')
add("AU_actor",P+"unit.go",r'project, string\(audit\.Actor\),','project, string(audit.Actor)+"x",')
add("REPLAY_short",P+"unit.go",r'\t\tif found \{\n\t\t\tif !sameContentHash\(existing\.ContentHash, record\.ContentHash\) \{','\t\tif false {\n\t\t\tif !sameContentHash(existing.ContentHash, record.ContentHash) {')
add("EXTRA_upd",P+"unit.go",r'(\tif err := tx\.Commit\(ctx\); err != nil \{\n\t\tif hasAbsenceInsert\(writes\) \{)',
 '\tif _, err := tx.Exec(ctx, store.Statement{SQL: "UPDATE tenants SET name = name || \'x\' WHERE id = $1", Args: []any{string(scope.TenantID)}}); err != nil {\n\t\treturn store.UnitResult{}, err\n\t}\n\\1')
add("MEM_ver",U+"identity/tenancy.go",r'"version":      int64\(unit\.Membership\.Version\),','"version":      int64(unit.Membership.Version) + 1,')
add("PRJ_ver",U+"identity/tenancy.go",r'"version":    int64\(unit\.Project\.Version\),','"version":    int64(unit.Project.Version) + 1,')
add("EXE_ver",U+"work/task.go",r'"version":          int64\(1\),','"version":          int64(2),')
add("CLN_ws",U+"execution/commit.go",r'values\["workspace_id"\] = nullableText\(string\(update\.WorkspaceID\)\)','values["workspace_id"] = nil')
add("MSG_author",U+"work/task.go",r'"author_id": string\(record\.Key\.PrincipalID\),','"author_id": "x",')
add("GW_model",  "internal/shell/gateway/units.go",r'"model":         record\.Model,','"model":         record.Model + "x",')
add("GW_retain", "internal/shell/gateway/units.go",r'"retain_until":  retainUntil,','"retain_until":  retainUntil.AddDate(0, 0, 1),')
add("BD_audit",U+"identity/binding.go",r'Audit:   append\(\[\]event\.Audit\(nil\), unit\.Audit\.\.\.\),','Audit:   nil,',2)
add("TRG_sel",U+"work/trigger.go",r'record\.Result = encoded','record.Result = append([]byte("{}"), encoded[:0]...)')
add("TA3",U+"work/task.go",r'Audit:   unit\.Audit,','Audit:   nil,',3)
add("TE4",U+"work/task.go",r'Events:  unit\.Events,','Events:  nil,',4)
add("TNE1",U+"identity/tenancy.go",r'Events:  append\(\[\]event\.Envelope\(nil\), unit\.Events\.\.\.\),','Events:  nil,',1)
add("VRE1",U+"platform/verify.go",r'Events:  append\(\[\]event\.Envelope\(nil\), unit\.Events\.\.\.\),','Events:  nil,',1)
# absence
add("ABS_nolock",P+"absence.go",r'const advisoryLockSQL = `SELECT pg_advisory_xact_lock\(hashtextextended\(\$1, 0\)\)`','const advisoryLockSQL = `SELECT 1 WHERE $1::text IS NOT NULL`')
add("ABS_noguard",P+"absence.go",r'SQL:  fmt\.Sprintf\("SELECT 1 FROM %s WHERE %s", lock\.Table, strings\.Join\(predicates, " AND "\)\),','SQL:  fmt.Sprintf("SELECT 1 FROM %s WHERE false AND %s", lock.Table, strings.Join(predicates, " AND ")),')
add("ABS_rawmap",P+"absence.go",r'if errors\.As\(err, &state\) && state\.SQLState\(\) == "23505" \{','if false \&\& errors.As(err, \&state) \&\& state.SQLState() == "23505" {')
# observer self mutations
add("OB_skiptable",S+"observer.go",r'(\tfor _, table := range store\.SchemaDesign\(\)\.Tables \{\n)(\t\trows, err := QueryStringsN)',r'\1\t\tif table.Name == "cleanup" {\n\t\t\tcontinue\n\t\t}\n\2')
add("OB_globalasserted",S+"observer.go",r'asserted: map\[string\]bool\{\},','asserted: sharedAsserted,')
add("OB_nocount",S+"observer.go",r'if mismatched := unitChangeCountMismatch\(counts, expected\); len\(mismatched\) > 0 \{','if mismatched := unitChangeCountMismatch(counts, expected); false \&\& len(mismatched) > 0 {')
add("OB_nogaps",S+"observer.go",r'if missing := unitChangeGaps\(changed, o\.asserted, o\.absent\); len\(missing\) > 0 \{','if missing := unitChangeGaps(changed, o.asserted, o.absent); false \&\& len(missing) > 0 {')
add("OB_aftereqbefore",S+"observer.go",r'after := o\.shard\.snapshotTables\(o\.t\)','after := o.before')
add("OB_deletes",S+"observer.go",r'if afterHash, ok := after\[table\]\[id\]; !ok \|\| afterHash != hash \{','if afterHash, ok := after[table][id]; ok \&\& afterHash != hash {')
add("OB_unexpected",S+"observer.go",r'\tfor table := range derived \{\n\t\tif _, ok := expected\[table\]; !ok \{\n\t\t\tmismatch = append\(mismatch, "unexpected "\+table\)\n\t\t\}\n\t\}\n','')
add("OB_gapsnil",S+"observer.go",r'\t\t\tif asserted\[key\] \|\| absent\[key\] \{\n\t\t\t\tcontinue\n\t\t\t\}\n\t\t\tgaps = append\(gaps, table\+"/"\+id\)','\t\t\t_ = key\n\t\t\tcontinue')
add("OB_idonly",S+"observer.go",r'observer\.asserted\[table\+"\\x00"\+id\] = true','observer.asserted[table+"\\x00"+id] = true; observer.asserted[table+"\\x00*"] = true')
add("GO_omitcol",S+"golden.go",r'if _, ok := want\[name\]; !ok \{\n\t\t\treturn fmt\.Errorf\("column %q is not stated in the expectation", name\)','if _, ok := want[name]; false \&\& !ok {\n\t\t\treturn fmt.Errorf("column %q is not stated in the expectation", name)')
add("GO_nullcmp",S+"golden.go",r'if isGoldenNull\(actual\) != isGoldenNull\(expected\) \{','if false \&\& isGoldenNull(actual) != isGoldenNull(expected) {')
add("GO_matcherempty",S+"golden.go",r'if fmt\.Sprint\(value\) == "" \{\n\t\t\treturn fmt\.Errorf\("value is empty"\)','if false {\n\t\t\treturn fmt.Errorf("value is empty")')
add("GW_at","internal/shell/gateway/types.go",r'"recorded_at":       record\.At,','"recorded_at":       record.At.AddDate(0, 0, 1),')
add("WS_task",U+"work/task.go",r'snapshotdoc\.DeriveWorkspaceID\(plan\.executionID\)','snapshotdoc.DeriveWorkspaceID(plan.executionID + "x")')
add("WS_exec",U+"execution/start.go",r'snapshotdoc\.DeriveWorkspaceID\(unit\.NewExecutionID\)','snapshotdoc.DeriveWorkspaceID(unit.NewExecutionID + "x")')
add("YAML_zero",U+"work/task.go",r'"yaml_sha256":      digest\[:\],','"yaml_sha256":      make([]byte, 32),')
add("CLI_intent",U+"execution/commit.go",r'values\["attempts"\] = int64\(0\)','values["attempts"] = int64(1)')
add("RCH_zero",P+"unit.go",r'contentHash, result, completed, execution, snapshot, record\.At','make([]byte, 32), result, completed, execution, snapshot, record.At')
