IMG ?= agent-deploy-java:latest
API_IMG ?= agent-deploy-java-api-server:latest

.PHONY: all build test docker-build docker-build-api-server

all: build

build:
	mvn clean package -DskipTests

test:
	mvn clean test

docker-build:
	docker build -t $(IMG) -f Dockerfile .

docker-build-api-server:
	docker build -t $(API_IMG) -f Dockerfile.api-server .

docker-build-all:
	docker build -t $(IMG) -f Dockerfile.unified .
