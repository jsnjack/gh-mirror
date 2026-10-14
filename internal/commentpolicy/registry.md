# Known AI reviewers

Registry version 3 recognizes Copilot, CodeRabbit, Gemini Code Assist and Claude using
exact, case-insensitive account aliases and GitHub's Bot account type. Copilot
GitHub.com account IDs 175728472 and 198982749 also recognize renamed accounts;
numeric IDs are never applied to another GitHub host. Copilot does not require a
`[bot]` suffix. Unknown accounts and missing account types remain unclassified.

Explicit final-line Claude Code attribution also identifies individual comments
posted by human accounts. Supported declarations include Addressed by, Generated
with/by, Reviewed by/with, Implemented by and Fixed by Claude Code, with optional
robot emoji, emphasis and canonical claude.ai/code or claude.com/claude-code links.
The exact final-line marker `<!-- gh-mirror:ai-generated:claude -->` is also supported.
Quoted, fenced, indented and inline-code examples do not count. A line that starts
with inline triple backticks, such as `` ```npm ci``` fails ``, does not open a fence. Mere mentions and
unmarked human comments remain unclassified. Attribution is a declaration, not
proof of authorship; it never classifies every comment from that person.

Provider documentation:

- [GitHub Copilot code review](https://docs.github.com/en/copilot/how-tos/use-copilot-agents/request-a-code-review/use-code-review)
- [CodeRabbit bot commands and custom service-account handles](https://docs.coderabbit.ai/guides/commands)
- [Gemini Code Assist review account](https://docs.cloud.google.com/gemini/docs/code-review/use-code-assist-github)
- [Claude Action authentication and account attribution](https://github.com/anthropics/claude-code-action/blob/main/docs/faq.md)

Custom accounts can be excluded explicitly by login. Include exceptions take
precedence over automatic, explicit-author and inherited thread exclusions.
Dependency, security and coverage automation is not classified as AI solely
because it is a bot. Detection performs no network requests or text inference.
