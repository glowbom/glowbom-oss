import { tool } from "@opencode-ai/plugin"

export default tool({
  description:
    "Ask Jev for a structured decision. Use for narrow judgments with clear choices, not for writing code or open-ended reasoning.",

  args: {
    state: tool.schema.string().describe("Facts or evidence Jev should judge"),
    question: tool.schema.string().describe("The question Jev should answer"),
    choices: tool.schema.string().describe(
      "At least 2 choices, one per line or separated by semicolons. Example: healthy=All checks pass; unhealthy=Checks fail"
    ),
  },

  async execute(args) {
    const criteria: Record<string, string> = {}

    for (const line of args.choices.split(/\n|;/)) {
      const i = line.indexOf("=")
      if (i <= 0) continue

      const key = line.slice(0, i).trim()
      const description = line.slice(i + 1).trim()

      if (key && description) {
        criteria[key] = description
      }
    }

    if (Object.keys(criteria).length < 2) {
      throw new Error(
        "Provide at least 2 choices, for example: healthy=All checks pass; unhealthy=Checks fail"
      )
    }

    const response = await fetch("https://opencode.ai/zen/v1/systemone", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
      },
      body: JSON.stringify({
        model: "jev-1.13-free",
        state: args.state,
        questions: {
          decision: {
            type: "choice",
            instructions: args.question,
            criteria,
          },
        },
      }),
    })

    if (!response.ok) {
      throw new Error(`Jev error ${response.status}: ${await response.text()}`)
    }

    return await response.text()
  },
})
