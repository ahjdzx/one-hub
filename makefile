NAME=one-api
DISTDIR=dist
WEBDIR=web
VERSION=$(shell git describe --tags || echo "dev")
GOBUILD=go build -ldflags "-s -w -X 'one-api/common/config.Version=$(VERSION)'"

all: one-api

web: $(WEBDIR)/build

$(WEBDIR)/build:
	cd $(WEBDIR) && yarn install && VITE_APP_VERSION=$(VERSION) yarn run build

one-api: web
	$(GOBUILD) -o $(DISTDIR)/$(NAME)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GOBUILD) -o $(DISTDIR)/$(NAME).linux

clean:
	rm -rf $(DISTDIR) && rm -rf $(WEBDIR)/build
