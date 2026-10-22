// An artifact's icon is one generic word, as with Claude's artifacts, or an emoji.
const iconWords = {
  chart: "📊", graph: "📈", dashboard: "📊", stats: "📈", calendar: "📅", schedule: "🗓️",
  recipe: "🍳", food: "🍽️", code: "💻", terminal: "⌨️", map: "🗺️", travel: "✈️",
  game: "🎮", puzzle: "🧩", music: "🎵", audio: "🎧", video: "🎬", photo: "📷",
  image: "🖼️", list: "📝", todo: "✅", checklist: "✅", note: "🗒️", notes: "🗒️",
  doc: "📄", document: "📄", book: "📚", money: "💰", finance: "💰", budget: "💰",
  timer: "⏱️", clock: "⏰", calculator: "🧮", math: "➗", science: "🔬", health: "❤️",
  fitness: "🏋️", home: "🏠", house: "🏠", server: "🖥️", network: "🌐", cloud: "☁️",
  weather: "🌤️", chat: "💬", mail: "✉️", email: "✉️", shop: "🛒", cart: "🛒",
  gift: "🎁", star: "⭐", idea: "💡", tool: "🔧", tools: "🧰", settings: "⚙️",
  data: "🗄️", table: "📋", form: "📝", quiz: "❓", slides: "📽️", presentation: "📽️",
  diagram: "🧭", flow: "🔀", art: "🎨", design: "🎨", team: "👥", people: "👥",
  user: "👤", pet: "🐾", plant: "🌱", garden: "🌱", car: "🚗", sport: "⚽",
  cards: "🃏", school: "🎓", work: "💼", security: "🔒", search: "🔍", rocket: "🚀",
};
function artifactIcon(icon) {
  if (!icon) return "";
  if (iconWords[icon]) return iconWords[icon];
  return /\p{Extended_Pictographic}/u.test(icon) && [...icon].length <= 4 ? icon : "";
}
// Shows the icon in the browser tab, or the app's own when there is none.
function setTabIcon(icon) {
  const link = document.querySelector('link[rel="icon"]');
  const emoji = artifactIcon(icon);
  link.href = emoji
    ? `data:image/svg+xml,${encodeURIComponent(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100"><text y=".9em" font-size="88">${emoji}</text></svg>`)}`
    : "/favicon.svg";
}
