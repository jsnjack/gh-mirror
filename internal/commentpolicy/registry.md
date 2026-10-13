# Known AI reviewers

Registry version 1 recognizes Copilot, CodeRabbit and Gemini Code Assist using
exact, case-insensitive account aliases and GitHub's Bot account type. Copilot
GitHub.com account IDs 175728472 and 198982749 also recognize renamed accounts;
numeric IDs are never applied to another GitHub host. Copilot does not require a
`[bot]` suffix. Unknown accounts and missing account types remain unclassified.

Provider documentation:

- [GitHub Copilot code review](https://docs.github.com/en/copilot/how-tos/use-copilot-agents/request-a-code-review/use-code-review)
- [CodeRabbit bot commands and custom service-account handles](https://docs.coderabbit.ai/guides/commands)
- [Gemini Code Assist review account](https://docs.cloud.google.com/gemini/docs/code-review/use-code-assist-github)

Custom accounts can be excluded explicitly by login. Include exceptions take
precedence over automatic, explicit-author and inherited thread exclusions.
Dependency, security and coverage automation is not classified as AI solely
because it is a bot. Detection performs no network requests or text inference.
