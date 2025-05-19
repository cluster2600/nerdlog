#!/bin/bash

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )"

# Define the paths for both directory trees
tests_output_root_dir="/tmp/nerdlog_core_test_output"
tests_input_root_dir="${SCRIPT_DIR}/../core/core_testdata/test_cases_core"

# Convert tests_output_root_dir to absolute path for consistency
tests_output_root_dir=$(realpath "$tests_output_root_dir")

# Recurse into subdirectories
for scenario_output_dir in "$tests_output_root_dir"/*; do
  scenario_name="${scenario_output_dir#$tests_output_root_dir/}"
  scenario_input_dir="$tests_input_root_dir/$scenario_name"

  for step_output_dir in "$scenario_output_dir"/steps/*; do
    if [ -e "$step_output_dir/got_log_resp.txt" ]; then
      # This is a query step, so copy the query output
      want_filename_ptr="$step_output_dir/want_log_resp_filename.txt"
      if ! [ -e "$want_filename_ptr" ]; then
        exit 1
      fi
      want_filename="$(cat $want_filename_ptr)"

      target_want_filename="$scenario_input_dir/$want_filename"
      if ! [ -e "$target_want_filename" ]; then
        echo "warn: $target_want_filename does not exist, skipping" 1>&2
        continue
      fi

      #echo "Copying $step_output_dir/got_log_resp.txt -> $target_want_filename"
      echo -n .
      cp "$step_output_dir/got_log_resp.txt" "$target_want_filename"
    fi
  done
done

echo ""
echo "All done"
