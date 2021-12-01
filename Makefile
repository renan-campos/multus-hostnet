all: /bin/controller /bin/setup-job /bin/teardown-job

/bin/controller: src/controller/main.go
	go build -o bin/controller src/controller/main.go

/bin/setup-job: src/setup-job/main.go
	go build -o bin/setup-job src/setup-job/main.go

/bin/teardown-job: src/teardown-job/main.go
	go build -o bin/teardown-job src/teardown-job/main.go

image: /bin/setup-job /bin/teardown-job /bin/controller
	docker build -t quay.io/rcampos/multus-hostnet:latest .

clean:
	rm -f bin/controller bin/job
