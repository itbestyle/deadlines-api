const tableBody = document.querySelector("#deadlineTable tbody");

// Функция для получения дедлайнов
async function fetchDeadlines(statusFilter = "", subjectFilter = "") {
    let url = "http://localhost:8080/deadlines";
    const params = new URLSearchParams();
    if (statusFilter) params.append("status", statusFilter);
    if (subjectFilter) params.append("subject", subjectFilter);
    if (params.toString()) url += "?" + params.toString();

    const res = await fetch(url);
    const deadlines = await res.json();

    tableBody.innerHTML = "";

    deadlines.forEach(d => {
        const row = document.createElement("tr");   
        row.innerHTML = `
            <td>${d.id}</td>
            <td>${d.title}</td>
            <td>${d.subject}</td>
            <td>${d.due_date}</td>
            <td>${d.status}</td>
            <td>
                <button onclick="editDeadline(${d.id})">Редактировать</button>
                <button onclick="deleteDeadline(${d.id})">Удалить</button>
            </td>
        `;
        tableBody.appendChild(row);
    });
}

// Вызываем при загрузке страницы
fetchDeadlines();

const addForm = document.getElementById("addForm");

addForm.addEventListener("submit", async (e) => {
    e.preventDefault(); // чтобы форма не перезагружала страницу

    const data = {
        title: document.getElementById("title").value,
        subject: document.getElementById("subject").value,
        due_date: document.getElementById("due_date").value,
        status: document.getElementById("status").value
    };

    await fetch("http://localhost:8080/deadlines", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(data)
    });

    // Обновляем таблицу
    fetchDeadlines();

    // Сбрасываем форму
    addForm.reset();
});
async function deleteDeadline(id) {
    await fetch(`http://localhost:8080/deadlines/${id}`, {
        method: "DELETE"
    });

    // Обновляем таблицу
    fetchDeadlines();
}
async function editDeadline(id) {
    // Получаем текущие значения
    const row = Array.from(tableBody.rows).find(r => r.cells[0].textContent == id);
    const currentTitle = row.cells[1].textContent;
    const currentSubject = row.cells[2].textContent;
    const currentDate = row.cells[3].textContent;
    const currentStatus = row.cells[4].textContent;

    // Спрашиваем пользователя новые значения
    const title = prompt("Название:", currentTitle) || currentTitle;
    const subject = prompt("Предмет:", currentSubject) || currentSubject;
    const due_date = prompt("Дата (YYYY-MM-DD):", currentDate) || currentDate;
    const status = prompt("Статус (в процессе, сдан, отменён):", currentStatus) || currentStatus;

    // Формируем объект для PUT-запроса
    const data = { title, subject, due_date, status };

    await fetch(`http://localhost:8080/deadlines/${id}`, {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(data)
    });

    // Обновляем таблицу
    fetchDeadlines();
}
const filterStatus = document.getElementById("filterStatus");
const filterSubject = document.getElementById("filterSubject");
const applyFilters = document.getElementById("applyFilters");

applyFilters.addEventListener("click", () => {
    fetchDeadlines(filterStatus.value, filterSubject.value);
});