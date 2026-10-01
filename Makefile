.PHONY: build clean

build:
	go build -o build/screenjson-db-importer .

clean:
	rm -rf build
