#!/bin/bash
# More reliable approach using commit templates

CONTEXT_FILE=".conversation-context.txt"
TEMPLATE_FILE=".git/commit-template.txt"

if [ "$#" -eq 0 ]; then
    echo "Usage: $0 \"commit message\""
    echo "Example: $0 \"Fix e2e testing infrastructure\""
    exit 1
fi

# Create commit template with user message and context
cat > "$TEMPLATE_FILE" << EOF
$*

EOF

# Add context if it exists
if [ -f "$CONTEXT_FILE" ] && [ -s "$CONTEXT_FILE" ]; then
    cat >> "$TEMPLATE_FILE" << EOF
User Requests Since Last Commit:
=================================
EOF
    cat "$CONTEXT_FILE" >> "$TEMPLATE_FILE"
fi

# Stage changes
git add -A

# Commit with template
git commit -t "$TEMPLATE_FILE"

# Clear context after successful commit
if [ $? -eq 0 ] && [ -f "$CONTEXT_FILE" ]; then
    > "$CONTEXT_FILE"
    echo "✓ Context cleared after successful commit"
fi

# Clean up template
rm -f "$TEMPLATE_FILE"

echo "✓ Committed with conversation context included" 