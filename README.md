# Storage

A GORM-style storage wrapper with unified API for different cloud storage backends

`storage.New(conf)` returns a `Storage` for S3 and S3-compatible stores, GCS, Azure, OCI, AliOSS or a local directory.

- `UploadData`, `UploadFile`, `DownloadData`, `DownloadFile` and `DeleteObject` upload and fetch whole files and return their location.
- Every other call takes a context and reports a missing or existing object as `ErrObjectNotFound` or `ErrObjectExists` (still wrapping the status code): `PutObject` and `UploadObject` can refuse to replace an object (`PutOptions.IfAbsent`, atomic), `GetObjectRange` and `DownloadObject` read a range or stream to any `io.Writer`, `DeleteObjects` succeeds for paths that hold nothing and fails on any key it could not delete.

The interface is also in `storage/objects`, which imports no provider SDK, for fakes and libraries that implement or accept it without creating one.
