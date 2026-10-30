import type { ThemeIconRole } from '$lib/themes';

/** The icon that tells the administrator what kind of tool a step used. */
export function toolIcon(name: string): ThemeIconRole {
	switch (name) {
		case 'repo_tree':
			return 'folder';
		case 'repo_read':
			return 'file';
		case 'repo_search':
		case 'api_search':
			return 'search';
		case 'repo_edit':
		case 'repo_write':
			return 'edit';
		case 'repo_delete':
			return 'delete';
		case 'pr_open':
		case 'pr_get':
		case 'pr_merge':
		case 'github_api':
			return 'github';
		case 'ci_status':
		case 'ci_logs':
		case 'prod_logs':
			return 'history';
		case 'ci_dispatch':
		case 'ci_control':
			return 'launch';
		case 'prod_status':
			return 'analytics';
		case 'db_query':
			return 'storage';
		case 'api_call':
			return 'plugin';
		default:
			return 'controls';
	}
}
