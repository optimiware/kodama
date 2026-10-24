# CLI and HTTP walkthrough

These are proposed commands, not an available release. Input files belong to a future tenant application repository. IDs/versions/digests are illustrative observed values.

```sh
kodama auth login --factory https://factory.example.invalid
kodama context use --tenant t_acme --project p_inventory
kodama work create --kind story --title "Resolve inventory exceptions" \
  --spec-file specification.md --acceptance-file acceptance.json --output json
kodama work update wi_1042 --spec-file revised-specification.md --if-version 1 --output json
kodama work transition wi_1042 --to ready --if-version 2 --reason "Input ready" --output json
kodama work transition wi_1042 --to active --if-version 3 --reason "Pull work" --output json
kodama workspace provision --work wi_1042 --output json
kodama workspace get ws_1042 --output json
```

Provision/read returns the Git branch and accepted commit. Humans clone that branch using their own authorized Git identity. Wait until the workspace is available before running it. Version numbers come from actual responses.

```sh
kodama pipeline validate --file pipeline.yaml --output json
kodama pipeline register --file pipeline.yaml --output json
kodama run start --work wi_1042 --definition pd_01 --content-revision 2 \
  --source-commit 1111111111111111111111111111111111111111 \
  --idempotency-key run-create-01 --output json
kodama run watch run_01 --output json
kodama run logs run_01 --step test --tail 100
kodama gate get gate_01 --output json
kodama gate decide gate_01 --decision approve --if-version 1 \
  --subject-digest sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
  --reason "Reviewed the current source and evidence" --idempotency-key decision-01 --output json
```

Inspect evidence and use the actual gate digest/version. The server derives reviewer authority from authentication. Reusing a key with changed content conflicts; an old key cannot bypass revoked permission.

HTTP equivalent of a versioned edit, assuming `ACCESS_TOKEN` is supplied by the supported login flow:

```sh
curl --fail-with-body -X PATCH \
  'https://factory.example.invalid/api/v1/tenants/t_acme/projects/p_inventory/work-items/wi_1042' \
  -H "Authorization: Bearer ${ACCESS_TOKEN}" \
  -H 'Content-Type: application/json' -H 'If-Match: "1"' \
  --data-binary '{"spec":"# Plan\nAdd the reviewed inventory exception flow."}'
```

This illustrates an alternative to the earlier edit, not another command to issue afterward. An uncertain response is resolved by reading the resource/history before retry.

```sh
kodama secret create erp-read-token --kind generic \
  --value-file /secure/input/erp-read-token --output json
kodama run start --work wi_1042 --definition pd_stage --content-revision 2 \
  --source-commit 2222222222222222222222222222222222222222 \
  --input image=art_image_01 --idempotency-key stage-run-01 --output json
```

Secret values never appear in management responses. The stage commit must match the verified image's provenance; using a different source requires a new build. Human completion is a separate versioned work transition after review.
