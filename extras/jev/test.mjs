const endpoint = "https://opencode.ai/zen/v1/systemone"

const response = await fetch(endpoint, {
  method: "POST",
  headers: {
    "Content-Type": "application/json",
  },
  body: JSON.stringify({
    model: "jev-1.13-free",
    state: "The build passed all tests.",
    questions: {
      status: {
        type: "choice",
        instructions: "What is the build status?",
        criteria: {
          good: "Successful or healthy",
          bad: "Failed or unhealthy",
        },
      },
    },
  }),
})

const text = await response.text()

if (!response.ok) {
  console.error(`Jev request failed (${response.status})`)
  console.error(text)
  process.exit(1)
}

try {
  console.log(JSON.stringify(JSON.parse(text), null, 2))
} catch {
  console.log(text)
}
