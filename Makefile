all: /bin/controller /bin/setup-job /bin/teardown-job

/bin/controller: cmd/controller/main.go
	go build -o bin/controller cmd/controller/main.go

/bin/setup-job: cmd/setup-job/main.go
	go build -o bin/setup-job cmd/setup-job/main.go

/bin/teardown-job: cmd/teardown-job/main.go
	go build -o bin/teardown-job cmd/teardown-job/main.go

image: /bin/setup-job /bin/teardown-job /bin/controller
	docker build -t quay.io/rcampos/multus-hostnet:latest .

push: image
	docker push quay.io/rcampos/multus-hostnet:latest

deploy: push
	kubectl apply -f deploy/

clean:
	rm -f bin/controller bin/job
