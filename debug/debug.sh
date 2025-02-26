#!/usr/bin/env bash
set -eu

export DOCKER_BUILDKIT=1 # Enable BuildKit engine.
export BUILDKIT_PROGRESS=plain # Set progress output to plain text.

# Check if we're running in non-native (privileged container) mode.
NON_NATIVE_MODE=false
if [[ "${1:-}" == "--non-native" ]]; then
    NON_NATIVE_MODE=true
    shift
fi

# Check if we're running inside a container and set variable.
if [ -f /.dockerenv ]; then
    INSIDE_CONTAINER=true
else
    INSIDE_CONTAINER=false
fi

# If not inside a container...
if [ "${INSIDE_CONTAINER}" != "true" ]; then
    OS="$(uname)"
    # If native mode is requested on a non-Linux host, exit.
    if [ "$OS" != "Linux" ] && [ "$NON_NATIVE_MODE" = false ]; then
        echo "Native build is supported only on Linux. On non-Linux hosts (like macOS), please run with --non-native for privileged container mode."
        exit 1
    fi

    # If running on Linux, perform the ptrace check.
    if [ "$OS" = "Linux" ]; then
        PTRACE_SCOPE_PROCFILE="/proc/sys/kernel/yama/ptrace_scope"
        if ! [ -f "$PTRACE_SCOPE_PROCFILE" ]; then
            echo "Unable to detect ${PTRACE_SCOPE_PROCFILE}, attempting to continue..."
        elif [ "$(<"$PTRACE_SCOPE_PROCFILE")" != "0" ]; then
            echo "You must set ${PTRACE_SCOPE_PROCFILE} to '0':"
            echo "echo 0 | sudo tee /proc/sys/kernel/yama/ptrace_scope"
            read -p "Do you want to do it now? (y/n) " -n 1 -r
            echo
            if [[ $REPLY =~ ^[Yy]$ ]]; then
                echo 0 | sudo tee /proc/sys/kernel/yama/ptrace_scope
            else
                exit 1
            fi
        fi
    fi
else
    echo "Running inside a container; skipping host-specific checks."
fi

# Check if the build-context directory exists.
DLV_CFG_DIR="${HOME}/.config/dlv"
if [ ! -d "$DLV_CFG_DIR" ]; then
    echo "The directory '$DLV_CFG_DIR' doesn't exist."
    read -p "Would you like to create it now? (y/n) " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        mkdir -p "$DLV_CFG_DIR"
        echo "Created directory '$DLV_CFG_DIR'."
    else
        echo "User declined to create the directory. Using fallback dummy build context..."
        TMP_DLV_CFG=$(mktemp -d)
        echo "# default dummy config" > "${TMP_DLV_CFG}/config.yml"
        DLV_CFG_DIR="$TMP_DLV_CFG"
    fi
fi

# Now check if config.yml exists inside the build-context directory.
if [ ! -f "${DLV_CFG_DIR}/config.yml" ]; then
    echo "File '${DLV_CFG_DIR}/config.yml' not found."
    read -p "Would you like to create a minimal default config file now? (y/n) " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        echo "# default dummy config" > "${DLV_CFG_DIR}/config.yml"
        echo "Created default config file."
    else
        echo "No config file provided. Exiting."
        exit 1
    fi
fi

PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROJECT_DIR="$(realpath "$PROJECT_DIR")"
cd "${PROJECT_DIR}"

if [ "$NON_NATIVE_MODE" = "true" ]; then
    # Build using your debug Dockerfile (privileged container mode)
    REF="docker-image://local/dalec/frontend:tmp"
    # When tagging, you may want to use the image name without the docker-image:// prefix.
    REF="local/dalec/frontend:tmp"
    docker build \
        -f debug/Dockerfile.debug \
        -t "${REF}" \
        --build-arg=HOSTDIR="${PROJECT_DIR}" \
        --build-context=dlv-cfg=${DLV_CFG_DIR} \
        .
    
    # Optionally forward the debug socket, etc.
    # (Your socat forwarding and subsequent build commands go here.)
    echo "DEBUG: REF=${REF}"
    docker build --build-arg=BUILDKIT_SYNTAX="${REF}" "$@"
else
    # Native build: only possible on Linux
    docker build "${1:-.}"
fi