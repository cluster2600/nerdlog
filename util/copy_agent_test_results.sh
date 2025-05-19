#!/bin/bash

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )"

# Define the paths for both directory trees
tests_output_root_dir="/tmp/nerdlog_agent_test_output"
tests_input_root_dir="${SCRIPT_DIR}/../core/core_testdata/test_cases_agent"

# Convert tests_output_root_dir to absolute path for consistency
tests_output_root_dir=$(realpath "$tests_output_root_dir")

# Function to copy stderr and stdout files recursively
copy_logs() {
  local current_dir="$1"

  # Get the relative path of current_dir from tests_output_root_dir
  relative_path="${current_dir#$tests_output_root_dir}"

  second_dir="$tests_input_root_dir/$relative_path"

  # Check if the corresponding directory exists in the second tree
  if [ ! -d "$second_dir" ]; then
    echo "Warning: Directory $second_dir does not exist. Skipping..."
    return
  fi

  # Check if the current directory has the expected files
  if [ -f "$current_dir/nerdlog_agent_stderr" ]; then
    echo -n .
    cp "$current_dir/nerdlog_agent_stderr" "$second_dir/want_stderr" || exit 1
  fi

  if [ -f "$current_dir/nerdlog_agent_stdout" ]; then
    echo -n .
    cp "$current_dir/nerdlog_agent_stdout" "$second_dir/want_stdout" || exit 1
  fi

  # Recurse into subdirectories
  for subdir in "$current_dir"/*; do
    if [ -d "$subdir" ] && [ "$(basename "$subdir")" != "journalctl_mock" ]; then
      copy_logs "$subdir" || exit 1
    fi
  done
}

# Start recursion from the top of the first tree
copy_logs "$tests_output_root_dir"
echo ""
echo "All done"
