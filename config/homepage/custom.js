// Lernae Custom Script - Font & Asset Loading
(function() {
  if (!document.getElementById("lernae-font")) {
    const link = document.createElement("link");
    link.id = "lernae-font";
    link.rel = "stylesheet";
    link.href = "https://fonts.googleapis.com/css2?family=Plus+Jakarta+Sans:wght@400;500;600;700&display=swap";
    document.head.appendChild(link);
  }
})();
