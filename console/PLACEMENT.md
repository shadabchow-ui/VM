# M7 Console — File Placement Guide

Place every file from this archive into the compute-platform repo at the paths below.
Run the commands in section 4 after placement.

## Backend files → services/resource-manager/

| This archive                           | Repo destination                                         | Action  |
|----------------------------------------|----------------------------------------------------------|---------|
| backend/instance_types.go              | services/resource-manager/instance_types.go              | Replace |
| backend/instance_handlers.go           | services/resource-manager/instance_handlers.go           | Replace |
| backend/api.go                         | services/resource-manager/api.go                         | Replace |
| backend/sshkey_handlers.go             | services/resource-manager/sshkey_handlers.go             | New     |
| backend/event_handlers.go              | services/resource-manager/event_handlers.go              | New     |
| backend/sshkey_repo.go                 | internal/db/sshkey_repo.go                               | New     |

## Frontend files → console/  (new directory at repo root)

| This archive                            | Repo destination                          |
|-----------------------------------------|-------------------------------------------|
| frontend/package.json                   | console/package.json                      |
| frontend/tsconfig.json                  | console/tsconfig.json                     |
| frontend/tsconfig.node.json             | console/tsconfig.node.json                |
| frontend/vite.config.ts                 | console/vite.config.ts                    |
| frontend/index.html                     | console/index.html                        |
| frontend/src/main.tsx                   | console/src/main.tsx                      |
| frontend/src/App.tsx                    | console/src/App.tsx                       |
| frontend/src/vite-env.d.ts              | console/src/vite-env.d.ts                 |
| frontend/src/api/client.ts              | console/src/api/client.ts                 |
| frontend/src/components/ActionButton.tsx| console/src/components/ActionButton.tsx   |
| frontend/src/components/CopyButton.tsx  | console/src/components/CopyButton.tsx     |
| frontend/src/components/Layout.tsx      | console/src/components/Layout.tsx         |
| frontend/src/components/Modal.tsx       | console/src/components/Modal.tsx          |
| frontend/src/components/Skeleton.tsx    | console/src/components/Skeleton.tsx       |
| frontend/src/components/StatusBadge.tsx | console/src/components/StatusBadge.tsx    |
| frontend/src/components/Toast.tsx       | console/src/components/Toast.tsx          |
| frontend/src/hooks/useEvents.ts         | console/src/hooks/useEvents.ts            |
| frontend/src/hooks/useInstance.ts       | console/src/hooks/useInstance.ts          |
| frontend/src/hooks/useInstances.ts      | console/src/hooks/useInstances.ts         |
| frontend/src/hooks/useSSHKeys.ts        | console/src/hooks/useSSHKeys.ts           |
| frontend/src/pages/CreateInstancePage.tsx | console/src/pages/CreateInstancePage.tsx |
| frontend/src/pages/InstanceDetailPage.tsx | console/src/pages/InstanceDetailPage.tsx |
| frontend/src/pages/InstancesListPage.tsx  | console/src/pages/InstancesListPage.tsx  |
| frontend/src/pages/SSHKeysPage.tsx      | console/src/pages/SSHKeysPage.tsx         |
| frontend/src/types/index.ts             | console/src/types/index.ts                |
| frontend/src/utils/format.ts            | console/src/utils/format.ts               |
| frontend/src/utils/states.ts            | console/src/utils/states.ts               |

## Run commands after placement

### Backend
```bash
# Verify compile — no changes to go.mod required
GOTOOLCHAIN=local go build ./services/resource-manager/...
GOTOOLCHAIN=local go vet  ./services/resource-manager/...
GOTOOLCHAIN=local go test ./services/resource-manager/...
GOTOOLCHAIN=local go test ./internal/db/...
```

### Frontend
```bash
cd console
npm install
npm run typecheck        # must pass with zero errors
npm run build            # must produce dist/

# Dev server (proxies /v1/* to resource-manager on :9090)
VITE_PRINCIPAL_ID=<your-principal-uuid> npm run dev
# → open http://localhost:3000
```

### Resource-manager runtime env for console dev
```bash
# Start resource-manager with CORS enabled (already in api.go)
DATABASE_URL=postgres://... REGION=us-east-1 go run ./services/resource-manager/
```
