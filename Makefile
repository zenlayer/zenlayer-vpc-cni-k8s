MODULE_NAME := $(shell head -n1 go.mod | sed -E 's/^module[[:space:]]*//; s/[[:space:]]*$$//')
# ?= so the image build can pass the value computed on the host: .dockerignore
# keeps .git out of the build context, so git inside the builder finds nothing.
COMMIT_ID ?= $(shell git rev-parse --short=8 HEAD 2>/dev/null || echo DEFAULT)
# VERSION identifies exactly which source a binary or image was built from.
#
# git describe, not a date plus a hash: the previous formula used
# `--contains HEAD`, which looks for a tag that DESCENDS from the commit. With
# no tags in the repository it degraded to the short SHA, so the version carried
# no release information at all while Chart.yaml separately claimed 1.0.0 --
# two version schemes that could not disagree loudly, only quietly.
#
#   v1.0.1              exactly on the tag
#   v1.0.1-7-g65b275b   seven commits past it
#   v1.0.1-7-g...-dirty built from a tree with uncommitted changes
#   65b275b             no tags in the repository yet
#
# --dirty is the part that earns its keep. An image built from a modified tree
# corresponds to no commit at all, and without the suffix nothing says so: a
# build was once traced back to its reported commit only by diffing the source
# line by line, because the binary contained code that commit did not have.
# It has a blind spot worth knowing -- untracked files do not make a tree dirty,
# though `go build ./...` compiles them -- so it answers "is this reproducible",
# not "did my fix get in". For the second question, check the binary itself.
#
# The leading v is kept: it is what the tags carry, what Chart.yaml's appVersion
# and values.yaml's image.tag declare, and the convention every image tag in this
# ecosystem follows. Stripping it here made the built image the only place the
# version appeared without it, so values.yaml pointed at a tag that did not
# exist. An untagged tree still falls back to a bare SHA, which has no v.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo unknown)
DATE_NOW := $(shell date "+%Y-%m-%d %H:%M:%S")
BRANCH ?= $(shell git rev-parse --abbrev-ref HEAD)

# Output Directory
OUTPUT ?= output

# Helm chart
CHART_DIR   ?= chart
CHART_OUTPUT ?= $(OUTPUT)/charts
HELM        ?= helm

# Container runtime engine
ENGINE ?= docker

# IMAGE Name and Tag
IMAGE_NAME ?= docker.io/zenlayer1/zeccni
IMAGE_TAG ?= $(VERSION)
IMAGE_NAME_TAG ?= $(IMAGE_NAME):$(IMAGE_TAG)
# The controlplane's own image (images/controlplane.Dockerfile): the node
# image's name with -controlplane, released with the same tag.
CONTROLPLANE_IMAGE_NAME ?= $(IMAGE_NAME)-controlplane
CONTROLPLANE_IMAGE_NAME_TAG ?= $(CONTROLPLANE_IMAGE_NAME):$(IMAGE_TAG)
# Chart version and appVersion default to whatever Chart.yaml declares, so a
# packaged chart stays self-consistent with the image tag in values.yaml.
# Override both when cutting a release.
CHART_VERSION ?= $(shell sed -n 's/^version:[[:space:]]*//p' $(CHART_DIR)/Chart.yaml | tr -d '[:space:]"')
CHART_APP_VERSION ?= $(shell sed -n 's/^appVersion:[[:space:]]*//p' $(CHART_DIR)/Chart.yaml | tr -d '[:space:]"')
# The image tag values.yaml points at -- the one version that decides what a
# cluster pulls, which is why version-check compares it too.
CHART_IMAGE_TAG ?= $(shell sed -n 's/^[[:space:]]*tag:[[:space:]]*//p' $(CHART_DIR)/values.yaml | head -1 | tr -d '[:space:]"')

# The release number a human maintains, taken from the newest CHANGELOG heading.
# VERSION above answers "which source is this"; this answers "which release is
# this meant to be". They are different questions, so they have different
# sources -- but they must not contradict each other, which is what
# version-check enforces.
# The \# is escaped: make treats an unescaped # as a comment even inside
# $(shell ...), which swallows the rest of the line including its closing paren.
CHANGELOG_VERSION ?= $(shell sed -n 's/^\#\# \[\([0-9][^]]*\)\].*/\1/p' CHANGELOG.md | head -1)

GOPROXY ?=
DEBUG ?= 1
FLAG=-gcflags="all=-trimpath=$(PWD)"
# Linker flags go into the single -ldflags of CNI_VERSION_LD_FLAG: go build
# keeps only the last -ldflags it is given, so a separate one is dropped.
LD_FLAGS=-s -w
ifeq ($(DEBUG), 1)
	FLAG=-gcflags="all=-N -l"
	LD_FLAGS=
endif
# CNIVersion carries the git describe output alone. The branch goes in its own
# variable rather than being appended as "<version>@<branch>": see the comment
# on version.CNIVersion for why.
CNI_VERSION_LD_FLAG=-ldflags="$(LD_FLAGS) -X '$(MODULE_NAME)/pkg/version.CNIVersion=$(VERSION)' -X '$(MODULE_NAME)/pkg/version.GitCommitID=$(COMMIT_ID)' -X '$(MODULE_NAME)/pkg/version.GitBranch=$(BRANCH)' -X '$(MODULE_NAME)/pkg/version.BuildTime=$(DATE_NOW)'"
# Only pass what is actually set: an empty --build-arg X= still lands as ENV X=""
# in the image. HTTP_PROXY used to be missing here entirely, and GOPROXY was
# passed twice.
BUILD_ARGS ?=
ifneq ($(strip $(HTTP_PROXY)),)
  BUILD_ARGS += --build-arg HTTP_PROXY=$(HTTP_PROXY)
endif
ifneq ($(strip $(HTTPS_PROXY)),)
  BUILD_ARGS += --build-arg HTTPS_PROXY=$(HTTPS_PROXY)
endif
ifneq ($(strip $(GOPROXY)),)
  BUILD_ARGS += --build-arg GOPROXY=$(GOPROXY)
endif

# Note: the bodies below must not be tab-indented. A tab-indented line outside a
# recipe is parsed as one, and the resulting "recipe commences before first
# target" masks the real problem -- which is what happened when go was missing
# from PATH and GOARCH came out empty.
GOARCH ?= $(shell go env GOARCH 2>/dev/null)

# Unconditional defaults: these used to be set only inside the amd64 branch, so
# on any other host they expanded to empty and the build args went out blank.
# .check-arch below is what actually restricts the build to amd64.
BASE_IMAGE ?= docker.io/ubuntu:26.04
BUILD_IMAGE ?= docker.io/library/golang:1.26

# Only the image build cares about the architecture. Keeping this out of the
# top level lets chart packaging and linting work on a machine without go.
.check-arch:
ifneq ($(GOARCH),amd64)
	@echo "unsupported GOARCH: '$(GOARCH)' (only amd64 is supported; is go on PATH?)"; exit 1
endif

help:
	@echo "\033[m┌─────────────────────────────────────────────────"
	@echo "\033[m│ * ModuleName:   \033[34m$(MODULE_NAME)"
	@echo "\033[m│ * GOARCH:       \033[34m$(GOARCH)"
	@echo "\033[m│ * BaseImage:    \033[34m$(BASE_IMAGE)"
	@echo "\033[m│ * BuildImage:   \033[34m$(BUILD_IMAGE)"
	@echo "\033[m│ * CommitId:     \033[34m$(COMMIT_ID)"
	@echo "\033[m│ * Version:      \033[34m$(VERSION)"
	@echo "\033[m│ * BRANCH:       \033[34m$(BRANCH)"
	@echo "\033[m│ * DATE_NOW:     \033[34m$(DATE_NOW)"
	@echo "\033[m│ * LD_FLAGS:     \033[34m$(LD_FLAGS)"
	@echo "\033[m│ * CNI_LD_FLAG:  \033[34m$(CNI_VERSION_LD_FLAG)"
	@echo "\033[m│ * BUILD_ARGS:   \033[34m$(BUILD_ARGS)"
	@echo "\033[m│ * MakeItem:     \033[34m\c"
	@make -qp | sed -nr 's/^([a-zA-Z][a-zA-Z0-9_-]*):([^=]|$$)/\1/p' | grep -v '^Makefile$$' | sort -u | tr '\n' ' ' && echo
	@echo "\033[m└─────────────────────────────────────────────────"

.PHONY: tidy .container-engine .helm .check-arch zec-cni zec-ipam zec-controlplane bin pkg image image-controlplane chart chart-lint chart-template version-check license license-check all clean

tidy:
	go mod tidy

.container-engine:
	@test -n "$(shell which $(ENGINE) 2>/dev/null)" || { echo "Missing container engine support, install Docker"; exit 1; }

.helm:
	@test -n "$(shell which $(HELM) 2>/dev/null)" || { echo "Missing helm, see https://helm.sh/docs/intro/install/"; exit 1; }


zec-cni:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -o $(OUTPUT)/zec-cni $(FLAG) $(CNI_VERSION_LD_FLAG) \
		./cmd/zenlayer-vpc-cni

zec-ipam:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -o $(OUTPUT)/zec-ipam $(FLAG) $(CNI_VERSION_LD_FLAG) \
		./cmd/cni-ipam

zec-controlplane:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -o $(OUTPUT)/zec-controlplane $(FLAG) $(CNI_VERSION_LD_FLAG) \
		./cmd/zec-controlplane

bin: tidy zec-cni zec-ipam zec-controlplane

pkg: bin
	cp ./script/bootstrap/* $(OUTPUT)/
	chmod +x $(OUTPUT)/*.sh

image: .container-engine .check-arch
	$(ENGINE) build -t ${IMAGE_NAME_TAG} -f ./images/Dockerfile \
		--build-arg BASE_IMAGE=$(BASE_IMAGE) --build-arg BUILD_IMAGE=$(BUILD_IMAGE) \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT_ID=$(COMMIT_ID) --build-arg BRANCH=$(BRANCH) \
		${BUILD_ARGS} .
	@echo "Built OCI image \"$(IMAGE_NAME)\""

image-controlplane: .container-engine .check-arch
	$(ENGINE) build -t ${CONTROLPLANE_IMAGE_NAME_TAG} -f ./images/controlplane.Dockerfile \
		--build-arg BUILD_IMAGE=$(BUILD_IMAGE) \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT_ID=$(COMMIT_ID) --build-arg BRANCH=$(BRANCH) \
		${BUILD_ARGS} .
	@echo "Built OCI image \"$(CONTROLPLANE_IMAGE_NAME)\""

# version-check fails the build when the four places a version appears drift
# apart: the CHANGELOG heading, Chart.yaml's appVersion, Chart.yaml's own
# version, and values.yaml's image.tag. Nothing enforced this before, and
# nothing would have reported it -- a chart claiming appVersion v1.0.0 packaged
# next to an image built from an untagged tree is a mismatch no test can see.
#
# image.tag is checked last but matters most: it is the only one of the four
# that decides what a cluster actually pulls. The comment above VERSION records
# the accident this guards against -- values.yaml once pointed at a tag that
# did not exist.
.PHONY: version-check
version-check:
	@cl='$(CHANGELOG_VERSION)'; \
	 app=$$(echo '$(CHART_APP_VERSION)' | sed 's/^v//'); \
	 chart='$(CHART_VERSION)'; \
	 tag=$$(echo '$(CHART_IMAGE_TAG)' | sed 's/^v//'); \
	 [ -n "$$cl" ] || { echo "version-check: no version heading found in CHANGELOG.md"; exit 1; }; \
	 [ -n "$$tag" ] || { echo "version-check: no image.tag found in $(CHART_DIR)/values.yaml"; exit 1; }; \
	 [ "$$cl" = "$$app" ] || { echo "version-check: CHANGELOG $$cl != Chart.yaml appVersion $$app"; exit 1; }; \
	 [ "$$cl" = "$$chart" ] || { echo "version-check: CHANGELOG $$cl != Chart.yaml version $$chart"; exit 1; }; \
	 [ "$$cl" = "$$tag" ] || { echo "version-check: CHANGELOG $$cl != values.yaml image.tag $$tag"; exit 1; }; \
	 echo "version-check: $$cl (build: $(VERSION))"

# License headers are the two-line SPDX form. addlicense only ever ADDS a header
# to a file that has none, so running `make license` cannot rewrite the year on a
# file that already carries one -- existing headers are left exactly as they are.
#
# Generated protobuf keeps whatever its generator emits, so rpc/*.pb.go is
# excluded; rpc/ipam.proto and rpc/generate.go are hand written and are not.
ADDLICENSE       ?= $(shell go env GOPATH)/bin/addlicense
ADDLICENSE_VER   ?= v1.2.0
LICENSE_HOLDER   ?= The Zenlayer Authors
LICENSE_YEAR     ?= 2026
LICENSE_IGNORE   := -ignore 'rpc/*.pb.go' -ignore '.tmp/**' -ignore 'tmp/**'
# The file list comes from git rather than find, so untracked scratch files are
# never rewritten. Outside a checkout -- a source tarball, for instance -- there
# is no list to build, and addlicense given no arguments prints its usage and
# exits non-zero, which would read as "these files have no header" when in fact
# nothing was examined. Both targets say they were skipped instead.
LICENSE_FILES     = $(shell git ls-files '*.go' '*.proto' 2>/dev/null)

.PHONY: .addlicense license license-check
.addlicense:
	@test -x '$(ADDLICENSE)' || go install github.com/google/addlicense@$(ADDLICENSE_VER)

license:
ifeq ($(strip $(LICENSE_FILES)),)
	@echo "license: skipped, not a git checkout"
else
	@$(MAKE) --no-print-directory .addlicense
	@'$(ADDLICENSE)' -s=only -c '$(LICENSE_HOLDER)' -l apache -y '$(LICENSE_YEAR)' \
	  $(LICENSE_IGNORE) $(LICENSE_FILES)
	@echo "license: headers present on $(words $(LICENSE_FILES)) files"
endif

license-check:
ifeq ($(strip $(LICENSE_FILES)),)
	@echo "license-check: skipped, not a git checkout"
else
	@$(MAKE) --no-print-directory .addlicense
	@'$(ADDLICENSE)' -check -s=only -c '$(LICENSE_HOLDER)' -l apache -y '$(LICENSE_YEAR)' \
	  $(LICENSE_IGNORE) $(LICENSE_FILES) \
	  || { echo "license-check: the files above have no license header; run 'make license'"; exit 1; }
	@echo "license-check: ok"
endif

# The behavior switches the chart requires; see the top of chart/values.yaml.
CHART_SWITCHES := --set zec.ipStack=ipv4 --set zec.manageUntaggedENI=false \
	--set cni.mode=chained --set cilium.enabled=false --set networkPolicy.enabled=false
CHART_IDS := --set zec.vpcID=vpc-test --set 'zec.subnetIDs={subnet-test}' --set zec.securityGroupID=sg-test
# Both IPAM modes, as CI renders them: crd adds the controlplane, which needs a
# region and the admission API it is gated on.
CHART_DIRECT := --set zec.ipamType=direct
CHART_CRD := --set zec.ipamType=crd --set controlplane.deployment.enabled=true \
	--set controlplane.regionID=region-test \
	--api-versions admissionregistration.k8s.io/v1/ValidatingAdmissionPolicy

# helm lint has no --api-versions, so the crd path (gated on the admission
# API) is checked by chart-template only.
chart-lint: .helm
	$(HELM) lint $(CHART_DIR) $(CHART_IDS) $(CHART_SWITCHES) $(CHART_DIRECT)

# Render with the required values filled in, so the `required`/`fail` guards in
# the templates do not mask real template errors.
chart-template: .helm
	@$(HELM) template zenlayer-vpc-cni $(CHART_DIR) $(CHART_IDS) $(CHART_SWITCHES) $(CHART_DIRECT)
	@$(HELM) template zenlayer-vpc-cni $(CHART_DIR) $(CHART_IDS) $(CHART_SWITCHES) $(CHART_CRD)

# Package the chart, stamping it with the same version the image gets.
chart: chart-lint
	@mkdir -p $(CHART_OUTPUT)
	$(HELM) package $(CHART_DIR) --destination $(CHART_OUTPUT) \
		--version $(CHART_VERSION) --app-version $(CHART_APP_VERSION)
	@echo "Packaged chart into $(CHART_OUTPUT)"

all: pkg image image-controlplane chart

clean:
	go clean -r -x
	rm -rf ./$(OUTPUT)
	docker rmi ${IMAGE_NAME_TAG} ${CONTROLPLANE_IMAGE_NAME_TAG}
