# Azure DevOps build-trigger examples

Two ways to start an Azure build on push, mirroring the Cloud Build and CodeBuild examples.

## 1. Incoming webhook (`azurewebhook`)

`azure-pipelines.yml` in your Azure repo:

```yaml
resources:
  webhooks:
    - webhook: BucketVCS
      connection: BucketVCSIncomingWebhook   # Incoming WebHook service connection

trigger: none

steps:
  - script: |
      echo "ref=${{ parameters.BucketVCS.ref_update.refname }}"
      echo "commit=${{ parameters.BucketVCS.head_oid }}"
    displayName: Show pushed commit
```

Register the trigger:

```bash
bucketvcs build trigger add --auth-db=$AUTH_DB \
  --tenant=acme --repo=app --name=azure-ci --kind=azurewebhook \
  --azure-webhook-url="https://dev.azure.com/$ORG/_apis/public/distributedtask/webhooks/BucketVCS?api-version=6.0-preview" \
  --secret="$WEBHOOK_SECRET" \
  --ref-include='refs/heads/main'
```

## 2. Run Pipeline REST (`azurepipelines`)

`--build-config` connector:

```yaml
build:
  azure_connectors:
    prod:
      org_url: https://dev.azure.com/MyOrg
      pat: ${AZURE_DEVOPS_PAT}
```

Register the trigger:

```bash
bucketvcs build trigger add --auth-db=$AUTH_DB \
  --tenant=acme --repo=app --name=azure-run --kind=azurepipelines \
  --azure-connector=prod --azure-project=MyProject --azure-pipeline-id=42 \
  --ref-include='refs/heads/main'
```

Your pipeline reads the push via the injected variables: `$(BV_REF)`, `$(BV_COMMIT)`, `$(BV_REPO)`, and (when token injection is on) `$(BVTS_TOKEN)` for cloning the repo.
