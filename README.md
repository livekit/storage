# Storage

Object storage on S3 and S3-compatible stores, GCS, Azure Blob, OCI Object Storage, AliOSS and a local directory, behind one interface. `storage.New(conf)` returns a `Storage`; every call takes a context.

- `Bucket` (`storage/objects`): `PutObject`, `UploadObject` (streams), `GetObject`, `GetObjectRange`, `DownloadObject` (to any `io.Writer`), `ObjectExists`, `ListObjects` (byte order), `DeleteObjects`, `PresignedURL`.
- `Storage` is `Bucket` plus `UploadFile` and `DownloadFile`. `DownloadFile` replaces the local file only once the whole object is read.
- `PutOptions{ContentType, IfAbsent}` on every write. `IfAbsent` refuses to replace an object, atomically where the store honors the condition.
- Errors: a missing object is `ErrNotFound`, a write `IfAbsent` refuses is `ErrExists` (`errors.Is`); `ErrorWithStatusCode` carries the store's HTTP status and `DeleteError` the paths a delete left (`errors.As`).
- `storage/objects` holds the interfaces in a module of its own, with no requirements (see Modules).

## Behavior

- **S3 semantics** where stores differ. `DeleteObjects` of a missing path succeeds, as S3's delete does. It tries every path, and when some are not deleted fails with a `*DeleteError` whose `Failed` names each one with its error (`errors.As`); every other path is deleted, and a retry is safe.
- **Keys** are the store's keys as given. The local directory, as a filesystem, drops a leading `/`, cleans `//` and `.`, refuses keys outside it, and cannot hold both `a` and `a/b`.
- **Timeouts**: a connection that moves no bytes either way for 5 minutes is cut; a slow but live transfer never is. The context bounds the call.
- **Retries**: every store retries throttling, 5xx and network errors with backoff, including AliOSS, whose SDK does not. A stream given to `UploadObject` is sent once on AliOSS.
- **Proxy**: `ProxyConfig` (S3, GCS) carries its credentials for http and https targets, honors `NO_PROXY`, and never proxies the cloud metadata services.
- **Local writes** go in place, as a file is written, so mounted volumes and file watchers see one write; `IfAbsent` writes a temporary file (`.upload-*`, never listed) and links it into place.

## Modules

| module | holds | requires |
|---|---|---|
| `github.com/livekit/storage` | the backends, `New`, the configs | every provider SDK, `livekit/protocol` |
| `github.com/livekit/storage/objects` | `Bucket`, `Storage`, `PutOptions`, the errors | nothing |
| `github.com/livekit/storage/e2e` | the end-to-end suites | `dockertest` |

Why `objects` is its own module:

- A module's requirements take part in the version selection of every module that requires it, whichever packages it imports. In the root module, the interfaces would carry every SDK's and `livekit/protocol`'s versions (grpc, otel) into each fake or library that implements or accepts them.
- With no requirements, implementing them costs no dependency: gosim's `fakes/objstore` `Bucket` is an `objects.Bucket`.

How its versions work:

- `go.mod` names the `objects` version the root requires; `go.work` builds the repo against `objects/` in the tree.
- A PR that changes `objects/` under a released version gets the next minor version committed to it by `objects-version.yaml`; pull that commit before pushing again. From a fork, CI fails and prints the command.
- Until the version is tagged, `go mod tidy` in the root fails on it (unknown revision); `go.work` builds the repo meanwhile.
- On merge, `objects-release.yaml` tags `objects/vX.Y.Z` on main; a failed run can be rerun by hand. Squash merges drop a branch's commits, so only a version tagged on main is sure to stay resolvable.
- `test` must be a required check on main with branches up to date: two PRs bumped to the same version cannot both merge then.

## Tests

The suites live in the `e2e` module, so this module's users do not inherit Docker's dependencies.

- `go test ./...` here runs the unit tests.
- `cd e2e && go test ./...` runs every backend except AliOSS end to end: against [floci](https://github.com/floci-io) emulators in Docker (S3, Azure Blob, GCS, OCI Object Storage), against MinIO for S3's conditional writes (`MINIO_BIN` or `minio` on `PATH`), and on the local directory. Without Docker or MinIO those tests skip, except when `CI` is set.
- The provider tests (`TestS3`, `TestGCP`, ...) reach real accounts when their environment variables are set; AliOSS has no emulator and is covered only that way.
