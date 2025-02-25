The build image currently can only be updated by a Grafana Mimir maintainer. If you're not a maintainer you can still open a PR with the changes, asking a maintainer to assist you publishing the updated image. The procedure is:

1. Update `mimir-build-image/Dockerfile`.
2. Build the and publish the image by using `make push-multiarch-build-image`. This will build and push multiplatform docker image (for linux/amd64 and linux/arm64). Pushing to `grafana/mimir-build-image` repository can only be done by a maintainer. Running this step successfully requires [Docker Buildx](https://docs.docker.com/buildx/working-with-buildx/), but does not require a specific platform.
3. Replace the image tag in `.github/workflows/*` (_there may be multiple references_) and Makefile (variable `LATEST_BUILD_IMAGE_TAG`).
4. Open a PR and make sure the CI with new build image passes
