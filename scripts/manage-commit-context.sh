#!/bin/bash
# Comprehensive script to manage conversation context for commit messages

CONTEXT_FILE=".conversation-context.txt"

usage() {
    echo "Usage: $0 [command] [options]"
    echo ""
    echo "Commands:"
    echo "  add \"prompt text\"     Add a user prompt to context"
    echo "  show                   Show current context"
    echo "  clear                  Clear current context"
    echo "  edit                   Edit context in $EDITOR"
    echo "  commit \"msg\"         Commit with context and clear"
    echo ""
    echo "Examples:"
    echo "  $0 add \"fix the remaining issues\""
    echo "  $0 add \"why is the main readme about openshift?\""
    echo "  $0 show"
    echo "  $0 commit \"Fix e2e testing infrastructure\""
}

add_prompt() {
    if [ -z "$1" ]; then
        echo "Error: No prompt text provided"
        usage
        exit 1
    fi
    
    local timestamp=$(date '+%Y-%m-%d %H:%M:%S')
    echo "[$timestamp] $*" >> "$CONTEXT_FILE"
    echo "✓ Added prompt to context: $*"
    echo "  Context will be included in next commit message."
}

show_context() {
    if [ -f "$CONTEXT_FILE" ] && [ -s "$CONTEXT_FILE" ]; then
        echo "Current conversation context:"
        echo "============================="
        cat "$CONTEXT_FILE"
    else
        echo "No conversation context stored."
    fi
}

clear_context() {
    if [ -f "$CONTEXT_FILE" ]; then
        > "$CONTEXT_FILE"
        echo "✓ Conversation context cleared."
    else
        echo "No context file to clear."
    fi
}

edit_context() {
    ${EDITOR:-nano} "$CONTEXT_FILE"
    echo "✓ Context file edited."
}

commit_with_context() {
    if [ -z "$1" ]; then
        echo "Error: No commit message provided"
        usage
        exit 1
    fi
    
    # Write the commit message to a temp file
    TEMP_MSG=$(mktemp)
    echo "$*" > "$TEMP_MSG"
    
    git add -A
    
    # Use git commit with -F to read from file, which will trigger prepare-commit-msg hook
    git commit -F "$TEMP_MSG"
    
    # Clean up temp file
    rm "$TEMP_MSG"
    
    echo "✓ Committed with context (context auto-appended by Git hook)"
}

case "$1" in
    add)
        shift
        add_prompt "$@"
        ;;
    show)
        show_context
        ;;
    clear)
        clear_context
        ;;
    edit)
        edit_context
        ;;
    commit)
        shift
        commit_with_context "$@"
        ;;
    *)
        usage
        exit 1
        ;;
esac 