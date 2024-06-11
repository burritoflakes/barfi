BINARY_NAME=barfi

export CGO_ENABLED=0

build:
	go build -ldflags="-s -w" -o $(BINARY_NAME) ./

build-linux:
	GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o ./$(BINARY_NAME) ./
