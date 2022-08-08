#!/bin/sh

# ./analyze.sh 改善しました 100

gh issue comment 1 --body "
## $1 $2

<details>
<summary>kataribe</summary>

\`\`\`
$(make kataru)
\`\`\`
</details>

<details>
<summary>slow-log</summary>

\`\`\`
$(make slow)
\`\`\`
</details>

$(git rev-parse HEAD)
"
