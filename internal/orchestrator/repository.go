package orchestrator

type TaskRepository struct {
	db DBTX
}

func NewTaskRepository(db DBTX) *TaskRepository {
	return &TaskRepository{db: db}
}

func (r *TaskRepository) CreateTask(task *Task) error {
	return CreateTask(r.db, task)
}

func (r *TaskRepository) GetTask(id string) (*Task, error) {
	return GetTask(r.db, id)
}

func (r *TaskRepository) ListTasks(filter TaskFilter) ([]*Task, error) {
	return ListTasks(r.db, filter)
}

func (r *TaskRepository) UpdateTask(task *Task) error {
	return UpdateTask(r.db, task)
}

func (r *TaskRepository) CreateAction(action *Action) error {
	return CreateAction(r.db, action)
}

func (r *TaskRepository) ListActionsByTask(taskID string) ([]*Action, error) {
	return ListActionsByTask(r.db, taskID)
}

type ProjectRepository struct {
	db DBTX
}

func NewProjectRepository(db DBTX) *ProjectRepository {
	return &ProjectRepository{db: db}
}

func (r *ProjectRepository) CreateProject(project *Project) error {
	return CreateProject(r.db, project)
}

func (r *ProjectRepository) GetProject(id string) (*Project, error) {
	return GetProject(r.db, id)
}

func (r *ProjectRepository) ListProjects() ([]*Project, error) {
	return ListProjects(r.db)
}

func (r *ProjectRepository) DeleteProject(id string) error {
	return DeleteProject(r.db, id)
}
