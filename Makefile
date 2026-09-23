.PHONY: build build-linux build-voice install install-voice test clean deps

PREFIX := $(CURDIR)/build/whisper
BUILD_DIR := $(CURDIR)/build/cmake
UNAME := $(shell uname)
PC_DIR := $(PREFIX)/lib/pkgconfig

# Default build: no voice/whisper support (works on all platforms)
build:
	go build -o ccc
	@if [ "$(UNAME)" = "Darwin" ]; then \
		codesign -f -s - ccc 2>/dev/null || true; \
	fi

# Cross-compiled Linux binary for the VPS. Pure Go (glebarez/sqlite +
# modernc), so CGO_ENABLED=0 works and the result has no libc dependency:
# scp it straight onto the box, no toolchain needed there.
build-linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ccc-linux-amd64
	@echo "✅ ccc-linux-amd64"

test:
	go build ./... && go vet ./... && go test ./...

# Build whisper.cpp C library (needed for voice support)
deps:
	@if [ ! -f "$(PREFIX)/lib/libwhisper.a" ]; then \
		echo "Building whisper.cpp..."; \
		git submodule update --init --recursive; \
		cmake -S third_party/whisper.cpp -B $(BUILD_DIR) \
			-DCMAKE_BUILD_TYPE=Release \
			-DBUILD_SHARED_LIBS=OFF \
			-DWHISPER_BUILD_TESTS=OFF \
			-DWHISPER_BUILD_EXAMPLES=OFF \
			-DWHISPER_BUILD_SERVER=OFF; \
		cmake --build $(BUILD_DIR) --config Release -j$$(nproc 2>/dev/null || sysctl -n hw.ncpu); \
		cmake --install $(BUILD_DIR) --prefix $(PREFIX); \
	else \
		echo "whisper.cpp already built"; \
	fi
	@# Generate pkg-config files matching go-whisper expectations
	@mkdir -p "$(PC_DIR)"
	@printf 'prefix=%s\nlibdir=$${prefix}/lib\nincludedir=$${prefix}/include\n\nName: libwhisper\nDescription: whisper.cpp\nVersion: 0.0.0\nCflags: -I$${includedir}\n' "$(PREFIX)" > "$(PC_DIR)/libwhisper.pc"
	@if [ "$(UNAME)" = "Darwin" ]; then \
		printf 'prefix=%s\nlibdir=$${prefix}/lib\n\nName: libwhisper-darwin\nDescription: whisper.cpp (darwin)\nVersion: 0.0.0\nLibs: -L$${libdir} -lwhisper -lggml -lggml-base -lggml-cpu -lggml-blas -lggml-metal -lstdc++ -framework Accelerate -framework Metal -framework Foundation -framework CoreGraphics\n' "$(PREFIX)" > "$(PC_DIR)/libwhisper-darwin.pc"; \
	else \
		printf 'prefix=%s\nlibdir=$${prefix}/lib\n\nName: libwhisper-linux\nDescription: whisper.cpp (linux)\nVersion: 0.0.0\nCflags: -fopenmp\nLibs: -L$${libdir} -lwhisper -lggml -lggml-base -lggml-cpu -lgomp -lm -lstdc++ -lpthread\n' "$(PREFIX)" > "$(PC_DIR)/libwhisper-linux.pc"; \
	fi

# Build with voice/whisper support (requires FFmpeg 7.x + cmake)
build-voice: deps
	PKG_CONFIG_PATH="$(PC_DIR)" CGO_LDFLAGS_ALLOW="-(W|D).*" \
		go build -tags voice -o ccc
	@if [ "$(UNAME)" = "Darwin" ]; then \
		codesign -f -s - ccc 2>/dev/null || true; \
	fi

install: build
	mkdir -p ~/bin
	install -m 755 ccc ~/bin/ccc
	@if [ "$(UNAME)" = "Darwin" ]; then \
		codesign -f -s - ~/bin/ccc 2>/dev/null || true; \
	fi
	@echo "✅ Installed to ~/bin/ccc — run: ccc restart"

install-voice: build-voice
	mkdir -p ~/bin
	install -m 755 ccc ~/bin/ccc
	@if [ "$(UNAME)" = "Darwin" ]; then \
		codesign -f -s - ~/bin/ccc 2>/dev/null || true; \
	fi
	@echo "✅ Installed to ~/bin/ccc (with voice support) — run: ccc restart"

clean:
	rm -f ccc ccc-linux-amd64
	rm -rf build/
