mkdir -p output
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o output/experiments ./bin/experiment
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o output/helpers ./bin/helper
docker-buildx build --pull --platform linux/amd64 -t wxt432/chaos-go-runner:dev -f build/Dockerfile.local --push .
rm -rf output