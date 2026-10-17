# Storage

A GORM-style storage wrapper with unified API for different cloud storage backends

Two interfaces over the same backends (S3 and S3-compatible, GCS, Azure, OCI, AliOSS, local):

| | `Storage` (`storage.New`) | `ObjectStorage` (`storage.NewObjectStorage`) |
|---|---|---|
| for | uploading files and handing out their location (egress, SIP) | services that keep and manage their own objects |
| context | none | every call |
| writes | replace | can refuse to replace (`PutOptions.IfAbsent`, atomic) |
| reads | whole object, to memory or a file | whole, a range, or streamed to any `io.Writer` |
| missing or existing object | `ErrorWithStatusCode` | `ErrObjectNotFound`, `ErrObjectExists` (still wrapping the status code) |
| a write returns | location URL and size | only an error |

`ObjectStorage` is also in `storage/objects`, which imports no provider SDK, for fakes and libraries that implement or accept it without creating one.
