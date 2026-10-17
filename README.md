# Storage

A GORM-style storage wrapper with unified API for different cloud storage backends

`storage.New(conf)` returns a `Storage` for S3 and S3-compatible stores, GCS, Azure, OCI, AliOSS or a local directory. Every call takes a context.

- `Bucket` (`storage/objects`): `PutObject`, `UploadObject` (can refuse to replace an object, `PutOptions.IfAbsent`), `GetObject`, `GetObjectRange`, `DownloadObject` (to any `io.Writer`), `ObjectExists`, `ListObjects`, `DeleteObjects`, `GeneratePresignedUrl`. A missing object is `ErrObjectNotFound` and a refused write `ErrObjectExists` (`errors.Is`).
- `Storage` is `Bucket` plus `UploadData`, `UploadFile` and `DownloadFile`, which publish and fetch whole files and return their location.
- `storage/objects` imports no provider SDK, so a fake or a library can implement or accept the interfaces without pulling in every SDK.
- The clients this package builds time out a connection that stalls before answering, never a transfer in progress; the context bounds the call.
