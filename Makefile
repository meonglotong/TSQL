GO ?= go
VERSION ?= 0.3.1
DISTDIR := dist

.PHONY: build test vet fmt e2e dist-tarball rpm

build:
	mkdir -p bin
	$(GO) build -o bin/tsqld ./cmd/tsqld
	$(GO) build -o bin/tsql ./cmd/tsql

test:
	$(GO) test ./... -count=1

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w .

e2e:
	./test/e2e.sh

# dist-tarball: source tarball expected by packaging/tsql.spec (Source0).
dist-tarball:
	rm -rf $(DISTDIR)
	mkdir -p $(DISTDIR)/tsql-$(VERSION)
	cp -R cmd docs go.mod internal test packaging LICENSE Makefile README.md $(DISTDIR)/tsql-$(VERSION)/
	cd $(DISTDIR) && tar czf tsql-$(VERSION).tar.gz tsql-$(VERSION)

# rpm: build the RPM into ~/rpmbuild/RPMS (needs rpmbuild + go on the host).
rpm: dist-tarball
	mkdir -p $(HOME)/rpmbuild/BUILD $(HOME)/rpmbuild/BUILDROOT $(HOME)/rpmbuild/SOURCES $(HOME)/rpmbuild/SPECS $(HOME)/rpmbuild/SRPMS $(HOME)/rpmbuild/RPMS
	cp $(DISTDIR)/tsql-$(VERSION).tar.gz $(HOME)/rpmbuild/SOURCES/
	rpmbuild --define "_topdir $(HOME)/rpmbuild" -bb packaging/tsql.spec
