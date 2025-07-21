#!/bin/bash
# Helper script to add user prompts to conversation context for commit messages

CONTEXT_FILE=".conversation-context.txt"

if [ "$#" -eq 0 ]; then
    echo "Usage: $0 \"user prompt text\""
    echo "Example: $0 \"fix the remaining issues\""
    echo ""
    echo "This adds the prompt to $CONTEXT_FILE which will be included in the next commit message"
    exit 1
fi

# Get current timestamp
TIMESTAMP=$(date '+%Y-%m-%d %H:%M:%S')

# Add the prompt to context file
echo "[$TIMESTAMP] $*" >> "$CONTEXT_FILE"

echo "Added prompt to context: $*"
echo "Will be included in next commit message." 