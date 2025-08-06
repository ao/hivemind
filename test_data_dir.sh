#!/bin/bash

# Test script for Hivemind data directory options

echo "Testing Hivemind data directory options"
echo "======================================="

# Build Hivemind if needed
if [ ! -f "./bin/hivemind" ]; then
  echo "Building Hivemind..."
  make build
fi

# Create test directories
mkdir -p ./test_data_dir_flag
mkdir -p ./test_data_dir_env

echo ""
echo "1. Testing with default data directory (should fail if not run as root):"
echo "----------------------------------------------------------------"
./bin/hivemind --help
echo ""
echo "To run with default directory: ./bin/hivemind"
echo "(Note: This will likely fail with 'permission denied' if not run as root)"

echo ""
echo "2. Testing with command-line flag:"
echo "--------------------------------"
echo "Command: ./bin/hivemind --data-dir ./test_data_dir_flag"
echo ""
echo "To test: ./bin/hivemind --data-dir ./test_data_dir_flag"

echo ""
echo "3. Testing with environment variable:"
echo "----------------------------------"
echo "Command: HIVEMIND_DATA_DIR=./test_data_dir_env ./bin/hivemind"
echo ""
echo "To test: HIVEMIND_DATA_DIR=./test_data_dir_env ./bin/hivemind"

echo ""
echo "4. Testing precedence (command-line flag overrides environment variable):"
echo "---------------------------------------------------------------------"
echo "Command: HIVEMIND_DATA_DIR=./test_data_dir_env ./bin/hivemind --data-dir ./test_data_dir_flag"
echo ""
echo "To test: HIVEMIND_DATA_DIR=./test_data_dir_env ./bin/hivemind --data-dir ./test_data_dir_flag"
echo "(This should use ./test_data_dir_flag as the data directory)"

echo ""
echo "Note: Press Ctrl+C to stop Hivemind after testing each option"