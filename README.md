# Storage

A GORM-style storage wrapper with unified API for different cloud storage backends

`storage.New(conf)` returns a `Storage` for S3 and S3-compatible stores, GCS, Azure, OCI, AliOSS or a local directory. Every call takes a context.

- `Bucket` (`storage/objects`): `PutObject`, `UploadObject` (can refuse to replace an object, `PutOptions.IfAbsent`), `GetObject`, `GetObjectRange`, `DownloadObject` (to any `io.Writer`), `ObjectExists`, `ListObjects`, `DeleteObjects`, `PresignedURL`. A missing object is `ErrNotFound` and a refused write `ErrExists` (`errors.Is`).
- `Storage` is `Bucket` plus `UploadFile` and `DownloadFile`, which publish and fetch whole files.
- Every backend follows S3's semantics where providers differ. `DeleteObjects` of a missing path succeeds, as S3's delete does, and fails while any path it was given still holds an object, so a retry after a partial failure is safe.
- `storage/objects` imports no provider SDK, so a fake or a library can implement or accept the interfaces without pulling in every SDK.
- The clients this package builds cut a connection that moves no bytes for 5 minutes, never a slow but live transfer; the context bounds the call.

## Tests

The suites live in the `e2e` module, so this module's users do not inherit Docker's dependencies.

- `go test ./...` here runs the unit tests.
- `cd e2e && go test ./...` runs every backend except AliOSS end to end: against [floci](https://github.com/floci-io) emulators in Docker (S3, Azure Blob, GCS, OCI Object Storage), against MinIO for S3's conditional writes (`MINIO_BIN` or `minio` on `PATH`), and on the local directory. Without Docker or MinIO those tests skip, except when `CI` is set.
- The provider tests (`TestS3`, `TestGCP`, ...) reach real accounts when their environment variables are set; AliOSS has no emulator and is covered only that way.
