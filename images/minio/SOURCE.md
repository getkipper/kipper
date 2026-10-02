# Source for this image

This image contains a program licensed under the GNU Affero General Public
License 3.0, built unmodified from its upstream source.

- MinIO server, release RELEASE.2025-09-07T16-13-09Z:
  https://github.com/minio/minio/tree/RELEASE.2025-09-07T16-13-09Z
- MinIO client (mc), release RELEASE.2025-08-13T08-35-41Z:
  https://github.com/minio/mc/tree/RELEASE.2025-08-13T08-35-41Z
- Build recipe (the Dockerfiles and the workflow that published this image),
  at the Kipper commit it was built from:
  https://github.com/getkipper/kipper/tree/KIPPER_REVISION/images/minio and
  https://github.com/getkipper/kipper/blob/KIPPER_REVISION/.github/workflows/build-minio-images.yml

The full licence is in /licenses/<program>/LICENSE and the licences of the
bundled dependencies are in /licenses/<program>/CREDITS.
