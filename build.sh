#!/bin/bash

BINFILE=lighthouse
if [ -n "$MSYSTEM" ]; then
    BINFILE=lighthouse.exe
fi
VERSION=$(git describe --tags)
echo "Building $VERSION..."
go build -o $BINFILE -ldflags "-X github.com/grioghar/lighthouse/internal/meta.Version=$VERSION"
